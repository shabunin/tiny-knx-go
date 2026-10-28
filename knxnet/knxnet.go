// Package knxnet encodes and decodes KNXnet/IP packets (KNX 03_08), and implements the client side
// of a tunnelling connection as a state machine without I/O. It does not use package net: the
// caller moves the packets, e.g. link/knxip over UDP.
package knxnet

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/shabunin/tiny-knx-go/cemi"
)

// Port is the standard KNXnet/IP UDP port.
const Port = 3671

// RoutingGroup is the standard multicast group for KNXnet/IP routing, 224.0.23.12.
var RoutingGroup = [4]byte{224, 0, 23, 12}

// ServiceID identifies the service carried by a packet.
type ServiceID uint16

// These are the services of KNXnet/IP core, tunnelling, routing and security.
const (
	SearchRequestID           ServiceID = 0x0201
	SearchResponseID          ServiceID = 0x0202
	DescriptionRequestID      ServiceID = 0x0203
	DescriptionResponseID     ServiceID = 0x0204
	ConnectRequestID          ServiceID = 0x0205
	ConnectResponseID         ServiceID = 0x0206
	ConnectionStateRequestID  ServiceID = 0x0207
	ConnectionStateResponseID ServiceID = 0x0208
	DisconnectRequestID       ServiceID = 0x0209
	DisconnectResponseID      ServiceID = 0x020A
	SearchRequestExtID        ServiceID = 0x020B
	SearchResponseExtID       ServiceID = 0x020C
	DeviceConfigRequestID     ServiceID = 0x0310
	DeviceConfigAckID         ServiceID = 0x0311
	TunnellingRequestID       ServiceID = 0x0420
	TunnellingAckID           ServiceID = 0x0421
	RoutingIndicationID       ServiceID = 0x0530
	RoutingLostMessageID      ServiceID = 0x0531
	RoutingBusyID             ServiceID = 0x0532
	SecureWrapperID           ServiceID = 0x0950
	SessionRequestID          ServiceID = 0x0951
	SessionResponseID         ServiceID = 0x0952
	SessionAuthenticateID     ServiceID = 0x0953
	SessionStatusID           ServiceID = 0x0954
	TimerNotifyID             ServiceID = 0x0955
	ObjectServerRequestID     ServiceID = 0xF080
	ObjectServerAckID         ServiceID = 0xF081
)

func (id ServiceID) String() string {
	return fmt.Sprintf("%#04x", uint16(id))
}

// A Service is the body of a packet.
type Service interface {
	ServiceID() ServiceID

	// appendBody appends the encoded body to b.
	appendBody(b []byte) []byte
}

const headerSize = 6

// The ObjectServer services of Weinzierl's BAOS have protocol version 2.0 in their header, the
// others 1.0.
const (
	protocolVersion             = 0x10
	objectServerProtocolVersion = 0x20
)

func headerVersion(id ServiceID) uint8 {
	if id == ObjectServerRequestID || id == ObjectServerAckID {
		return objectServerProtocolVersion
	}

	return protocolVersion
}

// These errors are returned by Decode.
var (
	ErrHeader    = errors.New("knxnet: invalid header")
	ErrTruncated = errors.New("knxnet: packet truncated")
	ErrStructure = errors.New("knxnet: invalid structure")
)

// Encode returns the packet for a service.
func Encode(s Service) []byte {
	b := s.appendBody(make([]byte, headerSize, 64))

	b[0] = headerSize
	b[1] = headerVersion(s.ServiceID())
	binary.BigEndian.PutUint16(b[2:], uint16(s.ServiceID()))
	binary.BigEndian.PutUint16(b[4:], uint16(len(b)))

	return b
}

// Decode parses a packet. Services that this package does not know are returned as *Unknown.
func Decode(data []byte) (Service, error) {
	if len(data) < headerSize || data[0] != headerSize {
		return nil, ErrHeader
	}

	id := ServiceID(binary.BigEndian.Uint16(data[2:]))
	if data[1] != protocolVersion && data[1] != headerVersion(id) {
		return nil, ErrHeader
	}

	total := int(binary.BigEndian.Uint16(data[4:]))

	if total < headerSize || total > len(data) {
		return nil, ErrTruncated
	}

	r := reader{data: data[headerSize:total]}

	var s Service

	switch id {
	case SearchRequestID:
		s = &SearchRequest{Discovery: r.hpai()}
	case SearchResponseID:
		s = &SearchResponse{Control: r.hpai(), Device: r.dibs()}
	case SearchRequestExtID:
		s = &SearchRequest{Discovery: r.hpai(), Extended: true}
		r.data = nil // search request parameters are not evaluated
	case SearchResponseExtID:
		s = &SearchResponse{Control: r.hpai(), Device: r.dibs(), Extended: true}
	case DescriptionRequestID:
		s = &DescriptionRequest{Control: r.hpai()}
	case DescriptionResponseID:
		s = &DescriptionResponse{Device: r.dibs()}
	case ConnectRequestID:
		s = r.connectRequest()
	case ConnectResponseID:
		s = r.connectResponse()
	case ConnectionStateRequestID:
		s = &ConnectionStateRequest{Channel: r.byte(), Control: r.skip(1).hpai()}
	case ConnectionStateResponseID:
		s = &ConnectionStateResponse{Channel: r.byte(), Status: Status(r.byte())}
	case DisconnectRequestID:
		s = &DisconnectRequest{Channel: r.byte(), Control: r.skip(1).hpai()}
	case DisconnectResponseID:
		s = &DisconnectResponse{Channel: r.byte(), Status: Status(r.byte())}
	case TunnellingRequestID:
		s = r.tunnellingRequest()
	case TunnellingAckID:
		s = r.tunnellingAck()
	case DeviceConfigRequestID:
		t := r.tunnellingRequest()
		s = &DeviceConfigRequest{Channel: t.Channel, Sequence: t.Sequence, Message: t.Message}
	case DeviceConfigAckID:
		t := r.tunnellingAck()
		s = &DeviceConfigAck{Channel: t.Channel, Sequence: t.Sequence, Status: t.Status}
	case ObjectServerRequestID:
		s = r.objectServerRequest()
	case ObjectServerAckID:
		t := r.tunnellingAck()
		s = &ObjectServerAck{Channel: t.Channel, Sequence: t.Sequence, Status: t.Status}
	case RoutingIndicationID:
		s = &RoutingIndication{Message: r.cemi()}
	case RoutingLostMessageID:
		r.expectLength(4)
		s = &RoutingLostMessage{DeviceState: r.byte(), Lost: r.uint16()}
	case RoutingBusyID:
		r.expectLength(6)
		s = &RoutingBusy{DeviceState: r.byte(), WaitTime: r.uint16(), Control: r.uint16()}
	case SecureWrapperID:
		s = r.secureWrapper()
	case SessionRequestID:
		s = r.sessionRequest()
	case SessionResponseID:
		s = r.sessionResponse()
	case SessionAuthenticateID:
		s = r.sessionAuthenticate()
	case SessionStatusID:
		s = &SessionStatus{Code: SessionCode(r.byte())}
		r.skip(1)
	default:
		s = &Unknown{ID: id, Body: append([]byte(nil), r.data...)}
	}

	if r.err != nil {
		return nil, r.err
	}

	return s, nil
}

// Status is the status code in responses.
type Status uint8

// These are the status codes of KNXnet/IP.
const (
	StatusOK                  Status = 0x00
	StatusHostProtocolType    Status = 0x01
	StatusVersionNotSupported Status = 0x02
	StatusSequenceNumber      Status = 0x04
	StatusConnectionID        Status = 0x21
	StatusConnectionType      Status = 0x22
	StatusConnectionOption    Status = 0x23
	StatusNoMoreConnections   Status = 0x24
	StatusNoMoreUniqueConns   Status = 0x25
	StatusDataConnection      Status = 0x26
	StatusKNXConnection       Status = 0x27
	StatusTunnellingLayer     Status = 0x29
)

var statusText = map[Status]string{
	StatusOK:                  "ok",
	StatusHostProtocolType:    "host protocol not supported",
	StatusVersionNotSupported: "version not supported",
	StatusSequenceNumber:      "wrong sequence number",
	StatusConnectionID:        "no connection with this channel",
	StatusConnectionType:      "connection type not supported",
	StatusConnectionOption:    "connection option not supported",
	StatusNoMoreConnections:   "no more connections",
	StatusNoMoreUniqueConns:   "no free individual address",
	StatusDataConnection:      "data connection error",
	StatusKNXConnection:       "KNX connection error",
	StatusTunnellingLayer:     "tunnelling layer not supported",
}

func (s Status) String() string {
	if text, ok := statusText[s]; ok {
		return text
	}

	return fmt.Sprintf("status %#02x", uint8(s))
}

// Error makes a non-OK status usable as an error.
func (s Status) Error() string {
	return "knxnet: " + s.String()
}

// Host protocols of an HPAI.
const (
	ProtocolUDP uint8 = 0x01
	ProtocolTCP uint8 = 0x02
)

// An HPAI is a host address. The zero HPAI (with protocol UDP) asks the server to answer to the
// address the request came from, which works through NAT ("route back").
type HPAI struct {
	Protocol uint8
	IP       [4]byte
	Port     uint16
}

// IsRouteBack reports whether the HPAI has no address.
func (h HPAI) IsRouteBack() bool {
	return h.IP == [4]byte{} && h.Port == 0
}

func appendHPAI(b []byte, h HPAI) []byte {
	if h.Protocol == 0 {
		h.Protocol = ProtocolUDP
	}

	b = append(b, 8, h.Protocol)
	b = append(b, h.IP[:]...)

	return binary.BigEndian.AppendUint16(b, h.Port)
}

// SearchRequest asks KNXnet/IP servers to describe themselves, sent to the routing group.
type SearchRequest struct {
	Discovery HPAI // where to send the responses

	// Extended marks a SEARCH_REQUEST_EXTENDED (KNXnet/IP 2), answered with an extended
	// response. Its search parameters are not supported.
	Extended bool
}

func (s *SearchRequest) ServiceID() ServiceID {
	if s.Extended {
		return SearchRequestExtID
	}

	return SearchRequestID
}

func (s *SearchRequest) appendBody(b []byte) []byte { return appendHPAI(b, s.Discovery) }

// SearchResponse describes a KNXnet/IP server.
type SearchResponse struct {
	Control  HPAI
	Device   DeviceInfo
	Extended bool // a SEARCH_RESPONSE_EXTENDED
}

func (s *SearchResponse) ServiceID() ServiceID {
	if s.Extended {
		return SearchResponseExtID
	}

	return SearchResponseID
}

func (s *SearchResponse) appendBody(b []byte) []byte {
	return s.Device.appendDIBs(appendHPAI(b, s.Control))
}

// DescriptionRequest asks a server for its description.
type DescriptionRequest struct {
	Control HPAI
}

func (*DescriptionRequest) ServiceID() ServiceID { return DescriptionRequestID }

func (s *DescriptionRequest) appendBody(b []byte) []byte { return appendHPAI(b, s.Control) }

// DescriptionResponse describes a server.
type DescriptionResponse struct {
	Device DeviceInfo
}

func (*DescriptionResponse) ServiceID() ServiceID { return DescriptionResponseID }

func (s *DescriptionResponse) appendBody(b []byte) []byte { return s.Device.appendDIBs(b) }

// DeviceInfo is the content of the device information and supported service families DIBs.
type DeviceInfo struct {
	Medium         uint8 // 0x02 = TP1, 0x20 = IP
	Status         uint8 // bit 0: programming mode
	Address        cemi.IndividualAddr
	ProjectID      uint16
	SerialNumber   [6]byte
	MulticastGroup [4]byte
	MAC            [6]byte
	Name           string // at most 30 bytes

	// Families maps service families (e.g. 0x04 tunnelling, 0x05 routing) to their versions.
	Families []ServiceFamily

	// Secured lists the service families that need a secure session (KNX IP Secure).
	Secured []ServiceFamily

	// ManufacturerData is the content of the manufacturer data DIB: the manufacturer ID and its
	// data, e.g. ObjectServerData. The DIB is left out if it is empty.
	ManufacturerData []byte
}

// A ServiceFamily is a group of services supported by a server, with its version.
type ServiceFamily struct {
	ID      uint8
	Version uint8
}

// Service families.
const (
	FamilyCore             uint8 = 0x02
	FamilyDeviceManagement uint8 = 0x03
	FamilyTunnelling       uint8 = 0x04
	FamilyRouting          uint8 = 0x05
	FamilySecurity         uint8 = 0x09

	// FamilyObjectServer is the ObjectServer protocol of Weinzierl's BAOS, whose version is
	// the major version of the protocol.
	FamilyObjectServer uint8 = 0xF0
)

// ObjectServerData returns the manufacturer data with which a BAOS tells in search responses that
// it serves the ObjectServer protocol, e.g. version 0x20 for 2.0: Weinzierl's manufacturer ID
// 0x00C5 and a record of type 1, F0 and the version.
func ObjectServerData(version uint8) []byte {
	return []byte{0x00, 0xC5, 0x01, 0x04, FamilyObjectServer, version}
}

// ObjectServer returns the version of the ObjectServer protocol (0x20 for 2.0) in the manufacturer
// data of a BAOS, or false.
func (d *DeviceInfo) ObjectServer() (uint8, bool) {
	data := d.ManufacturerData
	if len(data) < 2 || binary.BigEndian.Uint16(data) != 0x00C5 {
		return 0, false
	}

	for records := data[2:]; len(records) >= 2 && records[1] >= 2 && int(records[1]) <= len(records); records = records[records[1]:] {
		if records[0] == 1 && records[1] == 4 && records[2] == FamilyObjectServer {
			return records[3], true
		}
	}

	return 0, false
}

// Supports reports whether the server supports a service family.
func (d *DeviceInfo) Supports(family uint8) bool {
	for _, f := range d.Families {
		if f.ID == family {
			return true
		}
	}

	return false
}

const (
	dibDeviceInfo = 0x01
	dibFamilies   = 0x02
	dibSecured    = 0x06
	dibMfrData    = 0xFE
)

func (d *DeviceInfo) appendDIBs(b []byte) []byte {
	b = append(b, 54, dibDeviceInfo, d.Medium, d.Status)
	b = binary.BigEndian.AppendUint16(b, uint16(d.Address))
	b = binary.BigEndian.AppendUint16(b, d.ProjectID)
	b = append(b, d.SerialNumber[:]...)
	b = append(b, d.MulticastGroup[:]...)
	b = append(b, d.MAC[:]...)

	var name [30]byte
	copy(name[:], d.Name)
	b = append(b, name[:]...)

	b = appendFamilies(b, dibFamilies, d.Families)
	if len(d.Secured) > 0 {
		b = appendFamilies(b, dibSecured, d.Secured)
	}

	if len(d.ManufacturerData) > 0 {
		b = append(append(b, byte(2+len(d.ManufacturerData)), dibMfrData), d.ManufacturerData...)
	}

	return b
}

func appendFamilies(b []byte, dib uint8, families []ServiceFamily) []byte {
	b = append(b, byte(2+2*len(families)), dib)
	for _, f := range families {
		b = append(b, f.ID, f.Version)
	}

	return b
}

// ConnectionType selects what a connection is for.
type ConnectionType uint8

// Connection types.
const (
	DeviceManagementConnection ConnectionType = 0x03
	TunnelConnection           ConnectionType = 0x04

	// ObjectServerConnection carries the ObjectServer protocol of a BAOS (FamilyObjectServer).
	ObjectServerConnection ConnectionType = 0xF0
)

// TunnelLayer selects the layer of a tunnelling connection.
type TunnelLayer uint8

// Tunnelling layers.
const (
	TunnelLinkLayer  TunnelLayer = 0x02
	TunnelRaw        TunnelLayer = 0x04
	TunnelBusmonitor TunnelLayer = 0x80
)

// ConnectRequest opens a connection.
type ConnectRequest struct {
	Control HPAI
	Data    HPAI
	Type    ConnectionType
	Layer   TunnelLayer // for tunnel connections
}

func (*ConnectRequest) ServiceID() ServiceID { return ConnectRequestID }

func (s *ConnectRequest) appendBody(b []byte) []byte {
	b = appendHPAI(appendHPAI(b, s.Control), s.Data)

	if s.Type == TunnelConnection {
		return append(b, 4, byte(s.Type), byte(s.Layer), 0)
	}

	return append(b, 2, byte(s.Type))
}

// ConnectResponse answers a ConnectRequest. On success, Channel identifies the connection and, for
// tunnels, Address is the individual address the server assigned to it.
type ConnectResponse struct {
	Channel uint8
	Status  Status
	Data    HPAI
	Type    ConnectionType
	Address cemi.IndividualAddr
}

func (*ConnectResponse) ServiceID() ServiceID { return ConnectResponseID }

func (s *ConnectResponse) appendBody(b []byte) []byte {
	b = append(b, s.Channel, byte(s.Status))
	if s.Status != StatusOK {
		return b
	}

	b = appendHPAI(b, s.Data)

	if s.Type == TunnelConnection {
		b = append(b, 4, byte(s.Type))
		return binary.BigEndian.AppendUint16(b, uint16(s.Address))
	}

	return append(b, 2, byte(s.Type))
}

// ConnectionStateRequest is the heartbeat of a connection.
type ConnectionStateRequest struct {
	Channel uint8
	Control HPAI
}

func (*ConnectionStateRequest) ServiceID() ServiceID { return ConnectionStateRequestID }

func (s *ConnectionStateRequest) appendBody(b []byte) []byte {
	return appendHPAI(append(b, s.Channel, 0), s.Control)
}

// ConnectionStateResponse answers a ConnectionStateRequest.
type ConnectionStateResponse struct {
	Channel uint8
	Status  Status
}

func (*ConnectionStateResponse) ServiceID() ServiceID { return ConnectionStateResponseID }

func (s *ConnectionStateResponse) appendBody(b []byte) []byte {
	return append(b, s.Channel, byte(s.Status))
}

// DisconnectRequest closes a connection. Either side may send it.
type DisconnectRequest struct {
	Channel uint8
	Control HPAI
}

func (*DisconnectRequest) ServiceID() ServiceID { return DisconnectRequestID }

func (s *DisconnectRequest) appendBody(b []byte) []byte {
	return appendHPAI(append(b, s.Channel, 0), s.Control)
}

// DisconnectResponse answers a DisconnectRequest.
type DisconnectResponse struct {
	Channel uint8
	Status  Status
}

func (*DisconnectResponse) ServiceID() ServiceID { return DisconnectResponseID }

func (s *DisconnectResponse) appendBody(b []byte) []byte {
	return append(b, s.Channel, byte(s.Status))
}

// TunnellingRequest carries a cEMI message over a tunnel, in both directions.
type TunnellingRequest struct {
	Channel  uint8
	Sequence uint8
	Message  cemi.Message
}

func (*TunnellingRequest) ServiceID() ServiceID { return TunnellingRequestID }

func (s *TunnellingRequest) appendBody(b []byte) []byte {
	return appendCEMI(append(b, 4, s.Channel, s.Sequence, 0), s.Message)
}

// TunnellingAck acknowledges a TunnellingRequest.
type TunnellingAck struct {
	Channel  uint8
	Sequence uint8
	Status   Status
}

func (*TunnellingAck) ServiceID() ServiceID { return TunnellingAckID }

func (s *TunnellingAck) appendBody(b []byte) []byte {
	return append(b, 4, s.Channel, s.Sequence, byte(s.Status))
}

// DeviceConfigRequest carries a cEMI management message (M_PropRead and the like) over a device
// management connection, in both directions.
type DeviceConfigRequest struct {
	Channel  uint8
	Sequence uint8
	Message  cemi.Message
}

func (*DeviceConfigRequest) ServiceID() ServiceID { return DeviceConfigRequestID }

func (s *DeviceConfigRequest) appendBody(b []byte) []byte {
	return appendCEMI(append(b, 4, s.Channel, s.Sequence, 0), s.Message)
}

// DeviceConfigAck acknowledges a DeviceConfigRequest.
type DeviceConfigAck struct {
	Channel  uint8
	Sequence uint8
	Status   Status
}

func (*DeviceConfigAck) ServiceID() ServiceID { return DeviceConfigAckID }

func (s *DeviceConfigAck) appendBody(b []byte) []byte {
	return append(b, 4, s.Channel, s.Sequence, byte(s.Status))
}

// ObjectServerRequest carries a message of the ObjectServer protocol over an ObjectServer
// connection, in both directions: requests and indications to the server, responses and
// indications from it.
type ObjectServerRequest struct {
	Channel  uint8
	Sequence uint8
	Message  []byte
}

func (*ObjectServerRequest) ServiceID() ServiceID { return ObjectServerRequestID }

func (s *ObjectServerRequest) appendBody(b []byte) []byte {
	return append(append(b, 4, s.Channel, s.Sequence, 0), s.Message...)
}

// ObjectServerAck acknowledges an ObjectServerRequest.
type ObjectServerAck struct {
	Channel  uint8
	Sequence uint8
	Status   Status
}

func (*ObjectServerAck) ServiceID() ServiceID { return ObjectServerAckID }

func (s *ObjectServerAck) appendBody(b []byte) []byte {
	return append(b, 4, s.Channel, s.Sequence, byte(s.Status))
}

// RoutingIndication carries a cEMI message (L_Data.ind) to all routers and devices on the network.
type RoutingIndication struct {
	Message cemi.Message
}

func (*RoutingIndication) ServiceID() ServiceID { return RoutingIndicationID }

func (s *RoutingIndication) appendBody(b []byte) []byte { return appendCEMI(b, s.Message) }

// RoutingLostMessage tells that a router dropped messages because its queue was full.
type RoutingLostMessage struct {
	DeviceState uint8
	Lost        uint16
}

func (*RoutingLostMessage) ServiceID() ServiceID { return RoutingLostMessageID }

func (s *RoutingLostMessage) appendBody(b []byte) []byte {
	return binary.BigEndian.AppendUint16(append(b, 4, s.DeviceState), s.Lost)
}

// RoutingBusy asks all senders to pause, because a router's queue is filling up.
type RoutingBusy struct {
	DeviceState uint8
	WaitTime    uint16 // milliseconds
	Control     uint16
}

func (*RoutingBusy) ServiceID() ServiceID { return RoutingBusyID }

func (s *RoutingBusy) appendBody(b []byte) []byte {
	b = binary.BigEndian.AppendUint16(append(b, 6, s.DeviceState), s.WaitTime)
	return binary.BigEndian.AppendUint16(b, s.Control)
}

// Unknown is a service that this package does not decode.
type Unknown struct {
	ID   ServiceID
	Body []byte
}

func (s *Unknown) ServiceID() ServiceID { return s.ID }

func (s *Unknown) appendBody(b []byte) []byte { return append(b, s.Body...) }

func appendCEMI(b []byte, msg cemi.Message) []byte {
	n := len(b)
	b = append(b, make([]byte, cemi.Size(msg))...)
	cemi.Pack(b[n:], msg)

	return b
}

// reader decodes the body of a packet. After the first error, it returns zero values.
type reader struct {
	data []byte
	err  error
}

func (r *reader) fail(err error) {
	if r.err == nil {
		r.err = err
	}

	r.data = nil
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}

	if len(r.data) < n {
		r.fail(ErrTruncated)
		return nil
	}

	b := r.data[:n]
	r.data = r.data[n:]

	return b
}

func (r *reader) skip(n int) *reader {
	r.take(n)
	return r
}

func (r *reader) byte() uint8 {
	if b := r.take(1); b != nil {
		return b[0]
	}

	return 0
}

func (r *reader) uint16() uint16 {
	if b := r.take(2); b != nil {
		return binary.BigEndian.Uint16(b)
	}

	return 0
}

// expectLength reads a structure length byte and checks it.
func (r *reader) expectLength(n uint8) {
	if length := r.byte(); r.err == nil && length != n {
		r.fail(ErrStructure)
	}
}

func (r *reader) hpai() HPAI {
	var h HPAI

	r.expectLength(8)
	h.Protocol = r.byte()
	copy(h.IP[:], r.take(4))
	h.Port = r.uint16()

	return h
}

func (r *reader) cemi() cemi.Message {
	if r.err != nil {
		return nil
	}

	var msg cemi.Message
	if _, err := cemi.Unpack(r.data, &msg); err != nil {
		r.fail(err)
		return nil
	}

	r.data = nil

	return msg
}

func (r *reader) connectRequest() *ConnectRequest {
	s := &ConnectRequest{Control: r.hpai(), Data: r.hpai()}

	length := r.byte()
	s.Type = ConnectionType(r.byte())

	if length >= 4 {
		s.Layer = TunnelLayer(r.byte())
		r.skip(int(length) - 3)
	} else if length != 2 {
		r.fail(ErrStructure)
	}

	return s
}

func (r *reader) connectResponse() *ConnectResponse {
	s := &ConnectResponse{Channel: r.byte(), Status: Status(r.byte())}
	if s.Status != StatusOK || r.err != nil {
		return s
	}

	s.Data = r.hpai()

	length := r.byte()
	s.Type = ConnectionType(r.byte())

	if length >= 4 {
		s.Address = cemi.IndividualAddr(r.uint16())
		r.skip(int(length) - 4)
	} else if length != 2 {
		r.fail(ErrStructure)
	}

	return s
}

func (r *reader) tunnellingRequest() *TunnellingRequest {
	r.expectLength(4)
	s := &TunnellingRequest{Channel: r.byte(), Sequence: r.byte()}
	r.skip(1)
	s.Message = r.cemi()

	return s
}

func (r *reader) objectServerRequest() *ObjectServerRequest {
	r.expectLength(4)
	s := &ObjectServerRequest{Channel: r.byte(), Sequence: r.byte()}
	r.skip(1)

	if r.err == nil {
		s.Message = append([]byte(nil), r.data...)
		r.data = nil
	}

	return s
}

func (r *reader) tunnellingAck() *TunnellingAck {
	r.expectLength(4)

	return &TunnellingAck{Channel: r.byte(), Sequence: r.byte(), Status: Status(r.byte())}
}

// dibs decodes the description information blocks of search and description responses.
func (r *reader) dibs() DeviceInfo {
	var d DeviceInfo

	for r.err == nil && len(r.data) >= 2 {
		length := int(r.data[0])
		if length < 2 {
			r.fail(ErrStructure)
			break
		}

		block := r.take(length)
		if block == nil {
			break
		}

		switch block[1] {
		case dibDeviceInfo:
			if length < 54 {
				r.fail(ErrStructure)
				break
			}

			d.Medium = block[2]
			d.Status = block[3]
			d.Address = cemi.IndividualAddr(binary.BigEndian.Uint16(block[4:]))
			d.ProjectID = binary.BigEndian.Uint16(block[6:])
			copy(d.SerialNumber[:], block[8:14])
			copy(d.MulticastGroup[:], block[14:18])
			copy(d.MAC[:], block[18:24])
			d.Name = trimName(block[24:54])

		case dibFamilies:
			d.Families = families(block)

		case dibSecured:
			d.Secured = families(block)

		case dibMfrData:
			d.ManufacturerData = append([]byte(nil), block[2:]...)
		}
	}

	return d
}

func families(block []byte) []ServiceFamily {
	var f []ServiceFamily
	for i := 2; i+1 < len(block); i += 2 {
		f = append(f, ServiceFamily{ID: block[i], Version: block[i+1]})
	}

	return f
}

func trimName(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}

	return string(b)
}
