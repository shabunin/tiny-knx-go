package tpuart

// Services from the host to the chip (TP-UART2 and NCN5120 share these codes).
const (
	ResetReq         = 0x01
	StateReq         = 0x02
	SetAddressReq    = 0xF1 // followed by the address; the NCN5120 also wants a dummy byte
	DataStartReq     = 0x80 // U_L_DataStart/U_L_DataCont: 0x80 | position, then the frame byte
	DataEndReq       = 0x40 // U_L_DataEnd: 0x40 | position, then the checksum
	DataOffsetReq    = 0x08 // 0x08 | position>>6, for frame bytes beyond position 63
	AckInfoReq       = 0x10 // U_AckInformation: 0x10 | the flags below
	AckInfoAddressed = 0x01
	AckInfoBusy      = 0x02
	AckInfoNack      = 0x04
)

// Services from the chip to the host, apart from received frames.
const (
	ResetInd       = 0x03
	stateIndMask   = 0x07
	StateInd       = 0x07 // U_State.ind: error flags in the upper 5 bits
	configIndMask  = 0x83
	configInd      = 0x01 // U_Configure.ind (NCN5120)
	stopModeInd    = 0x2B
	ackIndMask     = 0x33
	ackInd         = 0x00 // L_Ackn.ind (bus monitor mode)
	confirmMask    = 0x7F
	ConfirmInd     = 0x0B // L_Data.con: bit 7 set when the frame was acknowledged
	confirmSuccess = 0x80
	pollDataInd    = 0xF0
	frameStateMask = 0x17
	frameStateInd  = 0x13
	frameEndInd    = 0xCB
	systemStatInd  = 0x4B
)

// Error flags of U_State.ind.
const (
	StateSlaveCollision = 0x80
	StateReceiveError   = 0x40
	StateTransmitError  = 0x20
	StateProtocolError  = 0x10
	StateTemperature    = 0x08
)

// appendSendRequest appends the U_L_Data services that transmit a complete frame (including its
// checksum): every byte is preceded by its position, the checksum by U_L_DataEnd.
func appendSendRequest(buf, frame []byte) []byte {
	for i, b := range frame {
		if offset := i >> 6; offset > 0 {
			buf = append(buf, DataOffsetReq|byte(offset))
		}

		position := byte(i & 0x3F)

		if i == len(frame)-1 {
			buf = append(buf, DataEndReq|position, b)
		} else {
			buf = append(buf, DataStartReq|position, b)
		}
	}

	return buf
}
