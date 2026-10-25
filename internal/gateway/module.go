package gateway

import (
	"fmt"
	"log"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/device"
	"github.com/shabunin/tiny-knx-go/link/ft12"
	"github.com/shabunin/tiny-knx-go/link/mock"
)

// StartModuleDevice hosts a device for the serial module on the bus, with the module's mask
// version and serial number; number is how the log names it. When ETS assigns it a new address,
// the module gets it too, so that it acknowledges the frames to it. A tunnel client at the module's
// address talks to it as alias, the address of the interface. It returns the device and a
// description of it.
func StartModuleDevice(bus *mock.Bus, number int, module *ft12.Link, tunnels []cemi.IndividualAddr, alias cemi.IndividualAddr) (*device.Device, string, error) {
	identity := device.Identity{MaskVersion: 0x0701} // EMI2 modules are BCU2

	mask, err := module.ReadProperty(cemi.ObjectTypeDevice, 1, cemi.PIDDeviceDescriptor, 1, 1)
	if err == nil && len(mask) == 2 {
		identity.MaskVersion = uint16(mask[0])<<8 | uint16(mask[1])
	}

	if serial, err := module.SerialNumber(); err == nil {
		identity.SerialNumber = serial
	}

	d, err := device.New(identity, device.Config{Address: module.Address()})
	if err != nil {
		return nil, "", err
	}

	d.OnAddressChange(func(addr cemi.IndividualAddr) {
		if err := module.SetAddress(addr); err != nil {
			log.Printf("Device %d: setting the address of the module to %v: %v", number, addr, err)
			log.Printf("Device %d: assign it through another interface: knxmon -serial-address %x:%v", number, identity.SerialNumber[:], addr)
			return
		}

		log.Printf("Device %d: address %v, the module has it too", number, addr)
	})
	d.OnProgModeChange(func(on bool) { log.Printf("Device %d: programming mode %v", number, on) })

	l := &moduleLink{
		Link:    bus.Attach(module.Address()),
		alias:   alias,
		inbound: make(chan *cemi.LDataInd, 64),
		peers:   map[cemi.IndividualAddr]time.Time{},
	}

	for _, t := range tunnels {
		if t == module.Address() {
			l.tunnel = t
		}
	}

	go l.filter()

	go func() {
		if err := d.Run(l); err != nil {
			log.Printf("Device %d: %v", number, err)
		}
	}()

	return d, fmt.Sprintf("%v module %04X", module.Protocol(), identity.MaskVersion), nil
}

// moduleLink attaches the module device to the bus. The device usually shares its address with the
// tunnel, and frames to that address may then be for either. The device gets broadcasts,
// connectionless frames and the connections that start with a T_Connect to it; the frames of the
// tunnel client's own connections stay away from it, as its transport layer would answer them with
// T_Disconnect.
//
// The tunnel client can talk to the device too, e.g. ETS connected through the module reading its
// identity. The device ignores frames from its own address, so the client appears to it as alias.
type moduleLink struct {
	*mock.Link
	tunnel  cemi.IndividualAddr // shared with the device, or zero
	alias   cemi.IndividualAddr
	inbound chan *cemi.LDataInd
	peers   map[cemi.IndividualAddr]time.Time // connected to the device, by last frame
}

// A transport connection closes after 6 seconds without frames.
const connectionTimeout = 6 * time.Second

func (l *moduleLink) filter() {
	for ind := range l.Link.Inbound() {
		if l.pass(ind, time.Now()) {
			l.inbound <- ind
		}
	}

	close(l.inbound)
}

func (l *moduleLink) pass(ind *cemi.LDataInd, now time.Time) bool {
	if l.tunnel == 0 || l.Address() != l.tunnel {
		return true
	}

	if ind.Source == l.tunnel {
		ind.Source = l.alias
		return true
	}

	if ind.Control2.IsGroupAddr() || cemi.IndividualAddr(ind.Destination) != l.tunnel {
		return true
	}

	control, ok := ind.Data.(*cemi.ControlData)
	if app, isApp := ind.Data.(*cemi.AppData); isApp && !app.Numbered {
		return true // connectionless
	}

	switch {
	case ok && !control.Numbered && control.Command == 0: // T_Connect
		l.peers[ind.Source] = now
		return true

	case ok && !control.Numbered && control.Command == 1: // T_Disconnect
		_, connected := l.peers[ind.Source]
		delete(l.peers, ind.Source)

		return connected
	}

	if last, connected := l.peers[ind.Source]; connected && now.Sub(last) < connectionTimeout {
		l.peers[ind.Source] = now
		return true
	}

	delete(l.peers, ind.Source)

	return false
}

// Send passes the frames of the device to the tunnel client, which it knows as alias.
func (l *moduleLink) Send(req *cemi.LDataReq) error {
	if l.tunnel != 0 && !req.Control2.IsGroupAddr() && cemi.IndividualAddr(req.Destination) == l.alias {
		req.Destination = uint16(l.tunnel)
	}

	return l.Link.Send(req)
}

func (l *moduleLink) Inbound() <-chan *cemi.LDataInd {
	return l.inbound
}
