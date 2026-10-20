package vbus

import (
	"sync"
	"time"
)

// The room model: each room loses heat to the outside in proportion to the temperature difference
// and gains the power of its heaters. With all heaters off a room cools down to Outside, with
// 2000 W it settles at 30 °C.
const (
	NumRooms = 4
	Outside  = 5.0 // °C

	lossRate = 1.0 / 600              // per second: the time constant of a room is 10 minutes
	heatRate = 25.0 * lossRate / 2000 // K per second per W
)

// Rooms simulates the temperature of the rooms. Speed runs the model faster than real time, so
// that the heating visibly works within minutes.
type Rooms struct {
	Speed float64

	mu      sync.Mutex
	temps   [NumRooms]float64
	heaters map[any]heater
}

type heater struct {
	room  int
	watts float64
}

// NewRooms returns rooms at 18 °C.
func NewRooms(speed float64) *Rooms {
	r := &Rooms{Speed: speed, heaters: map[any]heater{}}

	for i := range r.temps {
		r.temps[i] = 18
	}

	return r
}

// Temperature returns the temperature of room 1 to NumRooms.
func (r *Rooms) Temperature(room int) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.temps[index(room)]
}

// SetTemperature changes the temperature of a room, e.g. to open a window.
func (r *Rooms) SetTemperature(room int, temp float64) {
	r.mu.Lock()
	r.temps[index(room)] = temp
	r.mu.Unlock()
}

// Heat sets the power that the heater key gives to a room.
func (r *Rooms) Heat(key any, room int, watts float64) {
	r.mu.Lock()
	r.heaters[key] = heater{room: index(room), watts: watts}
	r.mu.Unlock()
}

// Step advances the model by d of real time.
func (r *Rooms) Step(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var power [NumRooms]float64
	for _, h := range r.heaters {
		power[h.room] += h.watts
	}

	dt := d.Seconds() * r.Speed

	for i := range r.temps {
		r.temps[i] += (power[i]*heatRate - (r.temps[i]-Outside)*lossRate) * dt
	}
}

// Run steps the model every second until stop is closed.
func (r *Rooms) Run(stop <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.Step(time.Second)
		case <-stop:
			return
		}
	}
}

// index converts a room number from 1 to a valid index.
func index(room int) int {
	return min(max(room, 1), NumRooms) - 1
}
