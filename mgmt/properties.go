package mgmt

import (
	"errors"
	"fmt"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/transport"
)

// A Property is a property of an interface object, with its description and, if read, its value.
type Property struct {
	Object     uint8  // index of the interface object
	ObjectType uint16 // e.g. 0 for the device object
	apdu.PropertyDescriptionResponse

	// Elements is the current number of elements, and Value holds the first of them (see
	// PropertyOptions.MaxElements). Err tells why the value could not be read.
	Elements uint16
	Value    []byte
	Err      error
}

// PropertyOptions configures ReadProperties.
type PropertyOptions struct {
	// Values selects whether the values are read, not only the descriptions.
	Values bool

	// MaxElements limits how many elements of a property are read. Zero means 16.
	MaxElements uint16

	// MaxObjects limits how many interface objects are read. Zero means 32.
	MaxObjects uint8
}

// ReadProperties reads the descriptions (and optionally the values) of all properties of all
// interface objects of the connected device. It stops at the first object index that has no
// PID_OBJECT_TYPE.
func ReadProperties(c *transport.Client, options PropertyOptions) ([]Property, error) {
	if options.MaxElements == 0 {
		options.MaxElements = 16
	}

	if options.MaxObjects == 0 {
		options.MaxObjects = 32
	}

	var properties []Property

	for object := uint8(0); object < options.MaxObjects; object++ {
		value, err := c.ReadProperty(object, PIDObjectType, 1, 1)
		if errors.Is(err, transport.ErrRejected) || err == nil && len(value) != 2 {
			break
		}

		if err != nil {
			return properties, err
		}

		objectType := uint16(value[0])<<8 | uint16(value[1])

		for index := 0; index < 256; index++ {
			description, err := c.DescribeProperty(object, 0, uint8(index))
			if err != nil {
				return properties, err
			}

			if description.MaxElements == 0 || description.PropertyID == 0 {
				break
			}

			p := Property{Object: object, ObjectType: objectType, PropertyDescriptionResponse: description}

			if options.Values {
				p.Elements, p.Value, p.Err = readValue(c, description, options.MaxElements)
				if p.Err != nil && !errors.Is(p.Err, transport.ErrRejected) {
					return properties, p.Err
				}
			}

			properties = append(properties, p)
		}
	}

	return properties, nil
}

// readValue reads the number of elements of a property and then its first elements, as many per
// request as fit in a standard frame.
func readValue(c *transport.Client, d apdu.PropertyDescriptionResponse, max uint16) (uint16, []byte, error) {
	count, err := c.ReadProperty(d.Object, d.PropertyID, 0, 1)
	if err != nil {
		return 0, nil, err
	}

	if len(count) != 2 {
		return 0, nil, transport.ErrRejected
	}

	elements := uint16(count[0])<<8 | uint16(count[1])
	n := elements

	if n > max {
		n = max
	}

	// A standard frame carries 10 bytes of property data.
	perRequest := uint16(1)
	if size := PDTSize(d.Type); size > 0 && size <= 10 {
		perRequest = uint16(10 / size)
	}

	var value []byte

	for start := uint16(1); start <= n; start += perRequest {
		chunk := n - start + 1
		if chunk > perRequest {
			chunk = perRequest
		}

		data, err := c.ReadProperty(d.Object, d.PropertyID, start, uint8(chunk))
		if err != nil {
			return elements, value, err
		}

		value = append(value, data...)
	}

	return elements, value, nil
}

// PIDObjectType is the property that every interface object has.
const PIDObjectType = 1

// PDTSize returns the size of one element of a property datatype, or 0 if it varies or is unknown.
func PDTSize(pdt uint8) int {
	switch {
	case pdt >= 0x11 && pdt <= 0x24: // PDT_GENERIC_01 to _20
		return int(pdt) - 0x10
	}

	return pdtSizes[pdt]
}

var pdtSizes = map[uint8]int{
	0x00: 1,  // control
	0x01: 1,  // char
	0x02: 1,  // unsigned char
	0x03: 2,  // int
	0x04: 2,  // unsigned int
	0x05: 2,  // KNX float
	0x06: 3,  // date
	0x07: 3,  // time
	0x08: 4,  // long
	0x09: 4,  // unsigned long
	0x0A: 4,  // float
	0x0B: 8,  // double
	0x0C: 10, // char block
	0x0D: 3,  // poll group setting
	0x0E: 5,  // short char block
	0x0F: 8,  // date time
	0x30: 2,  // version
	0x31: 6,  // alarm info
	0x32: 1,  // binary information
	0x33: 1,  // bitset8
	0x34: 2,  // bitset16
	0x35: 1,  // enum8
	0x36: 1,  // scaling
}

// ObjectTypeName returns the name of an interface object type.
func ObjectTypeName(objectType uint16) string {
	if name, ok := objectTypeNames[objectType]; ok {
		return name
	}

	return fmt.Sprintf("object type %d", objectType)
}

var objectTypeNames = map[uint16]string{
	0:  "device",
	1:  "address table",
	2:  "association table",
	3:  "application program",
	4:  "interface program",
	5:  "EIB object association table",
	6:  "router",
	7:  "LTE address routing table",
	8:  "cEMI server",
	9:  "group object table",
	10: "polling master",
	11: "KNXnet/IP parameter",
	13: "file server",
	17: "security",
	19: "RF medium",
}

// PropertyName returns the name of a property, e.g. "SERIAL_NUMBER". Property IDs above 50 depend
// on the object type; only those of the device object are known.
func PropertyName(objectType uint16, id uint8) string {
	if id <= 50 {
		if name, ok := generalPropertyNames[id]; ok {
			return name
		}
	} else if objectType == 0 {
		if name, ok := devicePropertyNames[id]; ok {
			return name
		}
	}

	return fmt.Sprintf("PID %d", id)
}

var generalPropertyNames = map[uint8]string{
	1:  "OBJECT_TYPE",
	2:  "OBJECT_NAME",
	3:  "SEMAPHOR",
	4:  "GROUP_OBJECT_REFERENCE",
	5:  "LOAD_STATE_CONTROL",
	6:  "RUN_STATE_CONTROL",
	7:  "TABLE_REFERENCE",
	8:  "SERVICE_CONTROL",
	9:  "FIRMWARE_REVISION",
	10: "SERVICES_SUPPORTED",
	11: "SERIAL_NUMBER",
	12: "MANUFACTURER_ID",
	13: "PROGRAM_VERSION",
	14: "DEVICE_CONTROL",
	15: "ORDER_INFO",
	16: "PEI_TYPE",
	17: "PORT_CONFIGURATION",
	18: "POLL_GROUP_SETTINGS",
	19: "MANUFACTURER_DATA",
	20: "ENABLE",
	21: "DESCRIPTION",
	22: "FILE",
	23: "TABLE",
	24: "ENROL",
	25: "VERSION",
	26: "GROUP_OBJECT_LINK",
	27: "MCB_TABLE",
	28: "ERROR_CODE",
	29: "OBJECT_INDEX",
	30: "DOWNLOAD_COUNTER",
}

var devicePropertyNames = map[uint8]string{
	51: "ROUTING_COUNT",
	52: "MAX_RETRY_COUNT",
	53: "ERROR_FLAGS",
	54: "PROGMODE",
	55: "PRODUCT_ID",
	56: "MAX_APDULENGTH",
	57: "SUBNET_ADDR",
	58: "DEVICE_ADDR",
	59: "PB_CONFIG",
	60: "ADDR_REPORT",
	61: "ADDR_CHECK",
	62: "OBJECT_VALUE",
	63: "OBJECTLINK",
	64: "APPLICATION",
	65: "PARAMETER",
	66: "OBJECTADDRESS",
	67: "PSU_TYPE",
	68: "PSU_STATUS",
	69: "PSU_ENABLE",
	70: "DOMAIN_ADDRESS",
	71: "IO_LIST",
	72: "MGT_DESCRIPTOR_01",
	73: "PL110_PARAM",
	74: "RF_REPEAT_COUNTER",
	75: "RECEIVE_BLOCK_TABLE",
	76: "RANDOM_PAUSE_TABLE",
	77: "RECEIVE_BLOCK_NR",
	78: "HARDWARE_TYPE",
	79: "RETRANSMITTER_NUMBER",
	80: "SERIAL_NR_TABLE",
	81: "BIBATMASTER_ADDRESS",
	82: "RF_DOMAIN_ADDRESS",
	83: "DEVICE_DESCRIPTOR",
	84: "METERING_FILTER_TABLE",
	85: "GROUP_TELEGR_RATE_LIMIT_TIME_BASE",
	86: "GROUP_TELEGR_RATE_LIMIT_NO_OF_TELEGR",
}
