package dpt

import "fmt"

// DPT_3007 represents DPT 3.007 / Control dimming: a direction and a step code. Step code 0 stops
// dimming; 1 to 7 dim by 100 %, 50 %, 25 %, … (100 % / 2^(StepCode-1)).
type DPT_3007 struct {
	Increase bool
	StepCode uint8
}

func (d DPT_3007) Pack() []byte {
	return []byte{packB1(d.Increase)<<3 | d.StepCode&7}
}

func (d *DPT_3007) Unpack(data []byte) error {
	if len(data) != 1 {
		return ErrInvalidLength
	}

	d.Increase = data[0]&8 != 0
	d.StepCode = data[0] & 7

	return nil
}

func (d DPT_3007) Unit() string {
	return ""
}

func (d DPT_3007) String() string {
	return controlString(d.Increase, d.StepCode, "increase", "decrease")
}

// DPT_3008 represents DPT 3.008 / Control blinds: a direction and a step code like DPT 3.007.
type DPT_3008 struct {
	Down     bool
	StepCode uint8
}

func (d DPT_3008) Pack() []byte {
	return []byte{packB1(d.Down)<<3 | d.StepCode&7}
}

func (d *DPT_3008) Unpack(data []byte) error {
	if len(data) != 1 {
		return ErrInvalidLength
	}

	d.Down = data[0]&8 != 0
	d.StepCode = data[0] & 7

	return nil
}

func (d DPT_3008) Unit() string {
	return ""
}

func (d DPT_3008) String() string {
	return controlString(d.Down, d.StepCode, "down", "up")
}

func controlString(set bool, step uint8, yes, no string) string {
	direction := no
	if set {
		direction = yes
	}

	if step == 0 {
		return direction + " stop"
	}

	return fmt.Sprintf("%s by step %d", direction, step)
}
