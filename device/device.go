// Package device implements a KNX device: group objects that are exchanged over group addresses,
// and the management services that ETS uses to find a device, read its identity and program its
// individual address.
//
// The configuration (individual address, group objects and their group addresses) is plain data
// (Config), so it can be set in code, loaded from a file or a web page, or, in future, downloaded
// by ETS.
package device

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/dpt"
	"github.com/shabunin/tiny-knx-go/link"
	"github.com/shabunin/tiny-knx-go/transport"
)

// A Device is a KNX device. Create it with New, attach callbacks and run it with Run.
type Device struct {
	mu sync.Mutex

	identity Identity
	address  cemi.IndividualAddr
	progMode bool
	objects  []*Object
	byGroup  map[cemi.GroupAddr][]*Object

	connConfig transport.Config
	conn       *transport.Conn
	link       link.Link // set by Run

	queue   []outgoing // frames waiting to be sent
	pending []func()   // callbacks to run once the lock is released
	wake    chan struct{}

	onAddressChange  func(cemi.IndividualAddr)
	onProgModeChange func(bool)
	onRestart        func()
}

type outgoing struct {
	req  *cemi.LDataReq
	conn *transport.Conn // the connection that sent the frame, if any
}

// New creates a device. A zero address in the configuration means DefaultAddress.
func New(identity Identity, config Config) (*Device, error) {
	if identity.MaskVersion == 0 {
		identity.MaskVersion = DefaultMaskVersion
	}

	if len(identity.OrderNumber) > 10 {
		return nil, errors.New("device: order number longer than 10 characters")
	}

	d := &Device{
		identity:   identity,
		address:    config.Address,
		byGroup:    make(map[cemi.GroupAddr][]*Object),
		connConfig: transport.DefaultConfig,
		wake:       make(chan struct{}, 1),
	}

	if d.address == 0 {
		d.address = DefaultAddress
	}

	d.conn = transport.NewConn(d.connConfig)

	for i, oc := range config.Objects {
		value := []byte{0}

		if oc.DPT != "" {
			datapoint, ok := dpt.Produce(oc.DPT)
			if !ok {
				return nil, fmt.Errorf("device: object %d: unknown DPT %q", i, oc.DPT)
			}

			value = datapoint.Pack()
		}

		oc.Addresses = append([]cemi.GroupAddr(nil), oc.Addresses...)
		obj := &Object{device: d, index: i, config: oc, value: value}
		d.objects = append(d.objects, obj)

		for _, ga := range oc.Addresses {
			if ga == 0 {
				return nil, fmt.Errorf("device: object %d: group address 0/0/0 is reserved", i)
			}

			d.byGroup[ga] = append(d.byGroup[ga], obj)
		}
	}

	return d, nil
}

// Objects returns the group objects in configuration order.
func (d *Device) Objects() []*Object {
	return d.objects
}

// Object returns the group object with the given name, or nil.
func (d *Device) Object(name string) *Object {
	for _, obj := range d.objects {
		if obj.config.Name == name {
			return obj
		}
	}

	return nil
}

// Address returns the current individual address.
func (d *Device) Address() cemi.IndividualAddr {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.address
}

// Config returns the current configuration, e.g. to save it after ETS changed the address.
func (d *Device) Config() Config {
	d.mu.Lock()
	defer d.mu.Unlock()

	config := Config{Address: d.address}
	for _, obj := range d.objects {
		config.Objects = append(config.Objects, obj.config)
	}

	return config
}

// ProgMode reports whether the device is in programming mode.
func (d *Device) ProgMode() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.progMode
}

// SetProgMode switches programming mode, usually from a button. In programming mode the device
// accepts a new individual address.
func (d *Device) SetProgMode(on bool) {
	d.locked(func(time.Time) { d.setProgMode(on) })
}

// OnAddressChange registers a function that is called when ETS assigns a new individual address.
// Save the configuration there.
func (d *Device) OnAddressChange(f func(cemi.IndividualAddr)) {
	d.mu.Lock()
	d.onAddressChange = f
	d.mu.Unlock()
}

// OnProgModeChange registers a function that is called when programming mode changes, e.g. to
// switch the programming LED.
func (d *Device) OnProgModeChange(f func(bool)) {
	d.mu.Lock()
	d.onProgModeChange = f
	d.mu.Unlock()
}

// OnRestart registers a function that is called when a restart is requested over the bus.
func (d *Device) OnRestart(f func()) {
	d.mu.Lock()
	d.onRestart = f
	d.mu.Unlock()
}

// Run connects the device to the bus and serves it until the link closes. If the link can change
// its individual address (link.AddressSetter), it is set to the device's address first.
func (d *Device) Run(l link.Link) error {
	d.mu.Lock()
	d.link = l
	addr := d.address
	d.mu.Unlock()

	if setter, ok := l.(link.AddressSetter); ok {
		if err := setter.SetAddress(addr); err != nil {
			return err
		}
	}

	// Objects with the I flag ask for their value at start.
	for _, obj := range d.objects {
		if obj.has(FlagCommunicate | FlagInit) {
			obj.RequestRead()
		}
	}

	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	for {
		d.flush(l)

		var tick <-chan time.Time

		d.mu.Lock()
		deadline, ok := d.conn.Deadline()
		d.mu.Unlock()

		if ok {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}

			timer.Reset(time.Until(deadline))
			tick = timer.C
		}

		select {
		case ind, open := <-l.Inbound():
			if !open {
				return link.ErrClosed
			}

			d.locked(func(now time.Time) { d.handle(now, ind) })

		case <-d.wake:

		case now := <-tick:
			d.locked(func(time.Time) {
				d.conn.Tick(now)
				d.processConn(now)
			})
		}
	}
}

// locked runs f with the lock held, then runs the callbacks it scheduled.
func (d *Device) locked(f func(now time.Time)) {
	d.mu.Lock()
	f(time.Now())
	pending := d.pending
	d.pending = nil
	d.mu.Unlock()

	for _, g := range pending {
		g()
	}
}

// later schedules a callback to run without the lock. The lock is held.
func (d *Device) later(f func()) {
	d.pending = append(d.pending, f)
}

// flush sends the queued frames.
func (d *Device) flush(l link.Link) {
	for {
		d.mu.Lock()
		if len(d.queue) == 0 {
			d.mu.Unlock()
			return
		}

		out := d.queue[0]
		d.queue = d.queue[1:]
		d.mu.Unlock()

		err := l.Send(out.req)

		if out.conn != nil {
			d.locked(func(now time.Time) {
				out.conn.HandleConfirm(now, out.req.Data, err == nil)

				if out.conn == d.conn {
					d.processConn(now)
				}
			})
		}
	}
}

// enqueue adds a frame to the send queue. The lock is held.
func (d *Device) enqueue(out outgoing) {
	d.queue = append(d.queue, out)

	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// send queues a frame from this device. The lock is held.
func (d *Device) send(dest uint16, group bool, prio cemi.Priority, tpdu cemi.TransportUnit, conn *transport.Conn) {
	control2 := cemi.Control2Hops(6)
	if group {
		control2 |= cemi.Control2GroupAddr
	}

	d.enqueue(outgoing{
		req: &cemi.LDataReq{LData: cemi.LData{
			Control1:    cemi.Control1Request(prio),
			Control2:    control2,
			Source:      d.address,
			Destination: dest,
			Data:        tpdu,
		}},
		conn: conn,
	})
}

func (d *Device) sendGroup(ga cemi.GroupAddr, a apdu.APDU) {
	d.send(uint16(ga), true, cemi.PrioLow, apdu.Encode(a), nil)
}

func (d *Device) sendBroadcast(a apdu.APDU) {
	d.send(0, true, cemi.PrioSystem, apdu.Encode(a), nil)
}

// handle processes a frame from the bus. The lock is held.
func (d *Device) handle(now time.Time, ind *cemi.LDataInd) {
	// Our own frames, or a device with the same address.
	if ind.Source == d.address {
		return
	}

	if ind.Control2.IsGroupAddr() {
		app, ok := ind.Data.(*cemi.AppData)
		if !ok {
			return
		}

		request, err := apdu.Decode(app)
		if err != nil {
			return
		}

		if ind.Destination == 0 {
			d.handleBroadcast(request)
		} else {
			d.handleGroup(cemi.GroupAddr(ind.Destination), request)
		}

		return
	}

	if cemi.IndividualAddr(ind.Destination) != d.address {
		return
	}

	if d.conn.HandleFrame(now, ind.Source, ind.Data) {
		d.processConn(now)
		return
	}

	// Connectionless point-to-point request.
	app, ok := ind.Data.(*cemi.AppData)
	if !ok {
		return
	}

	request, err := apdu.Decode(app)
	if err != nil {
		return
	}

	if response := d.handleManagement(request, false); response != nil {
		d.send(uint16(ind.Source), false, cemi.PrioLow, apdu.Encode(response), nil)
	}
}

func (d *Device) handleGroup(ga cemi.GroupAddr, request apdu.APDU) {
	objects := d.byGroup[ga]

	switch request := request.(type) {
	case apdu.GroupValueWrite:
		for _, obj := range objects {
			if obj.has(FlagCommunicate | FlagWrite) {
				obj.receive(request.Data)
			}
		}

	case apdu.GroupValueResponse:
		for _, obj := range objects {
			if obj.has(FlagCommunicate | FlagUpdate) {
				obj.receive(request.Data)
			}
		}

	case apdu.GroupValueRead:
		// Only one object should answer a group address.
		for _, obj := range objects {
			if obj.has(FlagCommunicate | FlagRead) {
				d.sendGroup(ga, apdu.GroupValueResponse{Data: obj.value})
				return
			}
		}
	}
}

// processConn handles the events of the transport connection. The lock is held.
func (d *Device) processConn(now time.Time) {
	conn := d.conn

	for d.conn == conn {
		event, ok := conn.NextEvent()
		if !ok {
			return
		}

		switch event.Kind {
		case transport.EventSend:
			d.send(uint16(event.Peer), false, event.Priority, event.TPDU, conn)

		case transport.EventData:
			request, err := apdu.Decode(event.TPDU.(*cemi.AppData))
			if err != nil {
				continue
			}

			if response := d.handleManagement(request, true); response != nil && d.conn == conn {
				conn.Send(now, apdu.Encode(response))
			}
		}
	}
}

func (d *Device) setProgMode(on bool) {
	if d.progMode == on {
		return
	}

	d.progMode = on

	if f := d.onProgModeChange; f != nil {
		d.later(func() { f(on) })
	}
}

func (d *Device) setAddress(addr cemi.IndividualAddr) {
	if addr == d.address || addr == 0 {
		return
	}

	d.address = addr

	if setter, ok := d.link.(link.AddressSetter); ok {
		d.later(func() { setter.SetAddress(addr) })
	}

	if f := d.onAddressChange; f != nil {
		d.later(func() { f(addr) })
	}
}

func (d *Device) restart(closeConnection bool) {
	d.setProgMode(false)

	// A restarted device forgets its connection without telling the peer.
	if closeConnection {
		d.conn = transport.NewConn(d.connConfig)
	}

	if f := d.onRestart; f != nil {
		d.later(f)
	}
}
