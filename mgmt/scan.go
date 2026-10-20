// Package mgmt implements management procedures that tools use on a KNX installation: finding the
// devices of a line and reading the interface objects of a device.
package mgmt

import (
	"time"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/link"
	"github.com/shabunin/tiny-knx-go/transport"
)

// ScanConfig configures Scan.
type ScanConfig struct {
	// Transport configures the connections to the probed devices. Short timeouts keep the scan
	// fast on links without acknowledgements (KNXnet/IP routing), where every absent device costs
	// AckTimeout × (MaxRepetitions + 1).
	Transport transport.Config

	// ResponseTimeout is how long to wait for the device descriptor after the device
	// acknowledged the request.
	ResponseTimeout time.Duration

	// Parallel is the number of devices probed at the same time.
	Parallel int

	// OnFound, if set, is called for every device as soon as it answered.
	OnFound func(Found)
}

// DefaultScanConfig probes 8 devices at a time, with 1 second acknowledgement timeouts.
var DefaultScanConfig = ScanConfig{
	Transport: transport.Config{
		ConnectionTimeout: 6 * time.Second,
		AckTimeout:        time.Second,
		MaxRepetitions:    1,
		DataPriority:      cemi.PrioLow,
	},
	ResponseTimeout: 2 * time.Second,
	Parallel:        8,
}

// Found is a device that answered a scan.
type Found struct {
	Address cemi.IndividualAddr

	// Mask is the mask version (device descriptor type 0), e.g. 0x07B0. It is zero if the device
	// acknowledged the request but did not answer it; Err tells why.
	Mask uint16
	Err  error
}

// LineAddresses returns the 256 addresses of a line, area.line.0 to area.line.255.
func LineAddresses(area, line uint8) []cemi.IndividualAddr {
	addrs := make([]cemi.IndividualAddr, 256)
	for i := range addrs {
		addrs[i] = cemi.NewIndividualAddr3(area, line, uint8(i))
	}

	return addrs
}

// Scan probes the devices at addrs: it connects to each, reads its device descriptor and
// disconnects. It returns the devices that answered, in address order. The scan owns the link
// while it runs: it reads all inbound frames.
//
// On media with acknowledgements (TP1, also through a BAOS module or a KNXnet/IP tunnel), absent
// devices are skipped as soon as the connect request is not acknowledged.
func Scan(l link.Link, addrs []cemi.IndividualAddr, config ScanConfig) ([]Found, error) {
	if config.ResponseTimeout <= 0 {
		config.ResponseTimeout = DefaultScanConfig.ResponseTimeout
	}

	if config.Parallel <= 0 {
		config.Parallel = DefaultScanConfig.Parallel
	}

	s := &scan{link: l, config: config, todo: addrs, probes: make(map[cemi.IndividualAddr]*probe)}

	return s.run()
}

type probe struct {
	conn      *transport.Conn
	connected bool
	answered  bool      // something came back from the device
	deadline  time.Time // for the descriptor, once the request was acknowledged
	result    *Found
}

type scan struct {
	link   link.Link
	config ScanConfig
	todo   []cemi.IndividualAddr
	probes map[cemi.IndividualAddr]*probe
	found  []Found
}

func (s *scan) run() ([]Found, error) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()

	defer timer.Stop()

	for {
		s.start(time.Now())

		if len(s.probes) == 0 {
			sortFound(s.found)
			return s.found, nil
		}

		var tick <-chan time.Time

		if deadline, ok := s.deadline(); ok {
			timer.Reset(time.Until(deadline))
			tick = timer.C
		}

		select {
		case ind, open := <-s.link.Inbound():
			if !open {
				return s.found, link.ErrClosed
			}

			s.receive(ind)

		case now := <-tick:
			for addr, p := range s.probes {
				p.conn.Tick(now)

				if !p.deadline.IsZero() && !now.Before(p.deadline) {
					p.result = &Found{Address: addr, Err: transport.ErrNoResponse}
					p.conn.Disconnect()
				}

				s.process(addr, p)
			}
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

// start begins probing the next addresses, up to the configured number in parallel.
func (s *scan) start(now time.Time) {
	for len(s.probes) < s.config.Parallel && len(s.todo) > 0 {
		addr := s.todo[0]
		s.todo = s.todo[1:]

		if self, ok := s.link.(link.Addresser); ok && self.Address() == addr {
			continue
		}

		p := &probe{conn: transport.NewConn(s.config.Transport)}
		s.probes[addr] = p

		p.conn.Connect(now, addr)
		s.process(addr, p)
	}
}

func (s *scan) deadline() (time.Time, bool) {
	var earliest time.Time

	for _, p := range s.probes {
		if deadline, ok := p.conn.Deadline(); ok && (earliest.IsZero() || deadline.Before(earliest)) {
			earliest = deadline
		}

		if !p.deadline.IsZero() && (earliest.IsZero() || p.deadline.Before(earliest)) {
			earliest = p.deadline
		}
	}

	return earliest, !earliest.IsZero()
}

// receive passes a frame to the probe of its sender.
func (s *scan) receive(ind *cemi.LDataInd) {
	if ind.Control2.IsGroupAddr() {
		return
	}

	if self, ok := s.link.(link.Addresser); ok {
		if addr := self.Address(); addr != 0 && cemi.IndividualAddr(ind.Destination) != addr {
			return
		}
	}

	p, ok := s.probes[ind.Source]
	if !ok {
		return
	}

	p.answered = true
	p.conn.HandleFrame(time.Now(), ind.Source, ind.Data)

	s.process(ind.Source, p)
}

// process carries out the events of a probe's connection.
func (s *scan) process(addr cemi.IndividualAddr, p *probe) {
	for {
		event, ok := p.conn.NextEvent()
		if !ok {
			return
		}

		now := time.Now()

		switch event.Kind {
		case transport.EventSend:
			err := s.link.Send(&cemi.LDataReq{LData: cemi.LData{
				Control1:    cemi.Control1Request(event.Priority),
				Control2:    cemi.Control2Hops(6),
				Destination: uint16(addr),
				Data:        event.TPDU,
			}})

			p.conn.HandleConfirm(time.Now(), event.TPDU, err == nil)

		case transport.EventConnected:
			p.connected = true
			p.conn.Send(now, apdu.Encode(apdu.DeviceDescriptorRead{}))

		case transport.EventConfirmed:
			p.deadline = now.Add(s.config.ResponseTimeout)

		case transport.EventData:
			response, err := apdu.Decode(event.TPDU.(*cemi.AppData))
			if d, ok := response.(apdu.DeviceDescriptorResponse); err == nil && ok && p.result == nil {
				p.result = &Found{Address: addr}

				if d.Type == 0 && len(d.Descriptor) >= 2 {
					p.result.Mask = uint16(d.Descriptor[0])<<8 | uint16(d.Descriptor[1])
				} else {
					p.result.Err = transport.ErrRejected
				}

				p.deadline = time.Time{}
				p.conn.Disconnect()
			}

		case transport.EventDisconnected:
			delete(s.probes, addr)

			// A device that never answered is absent: on media without acknowledgements, a
			// connection to any address seems to succeed.
			if p.result == nil && p.answered {
				p.result = &Found{Address: addr, Err: transport.ErrNoResponse}
			}

			if p.result != nil {
				s.found = append(s.found, *p.result)

				if s.config.OnFound != nil {
					s.config.OnFound(*p.result)
				}
			}
		}
	}
}

func sortFound(found []Found) {
	for i := 1; i < len(found); i++ {
		for j := i; j > 0 && found[j].Address < found[j-1].Address; j-- {
			found[j], found[j-1] = found[j-1], found[j]
		}
	}
}
