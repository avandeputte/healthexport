package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// Mirrors Shared/SyncProtocol.swift. Swift's Codable encodes each enum case as a one-key object:
// unlabeled associated values as {"case":{"_0":{...}}}, labeled ones as {"case":{"label":...}}.
// Dates are ISO 8601 without fractional seconds; nil optionals are omitted.

type Source struct {
	Name string `json:"name"`
}

type Hello struct {
	ProtocolVersion int    `json:"protocolVersion"`
	DeviceName      string `json:"deviceName"`
	SystemVersion   string `json:"systemVersion"`
	FullResync      bool   `json:"fullResync"`
}

type Sample struct {
	UUID     string            `json:"uuid"`
	Type     string            `json:"type"`
	Kind     string            `json:"kind"`
	Start    time.Time         `json:"start"`
	End      time.Time         `json:"end"`
	Value    *float64          `json:"value"`
	Unit     *string           `json:"unit"`
	Text     *string           `json:"text"`
	Count    *int64            `json:"count"`
	Source   Source            `json:"source"`
	Metadata map[string]string `json:"metadata"`
}

type Workout struct {
	UUID            string            `json:"uuid"`
	ActivityType    string            `json:"activityType"`
	ActivityTypeRaw int               `json:"activityTypeRaw"`
	Start           time.Time         `json:"start"`
	End             time.Time         `json:"end"`
	Duration        float64           `json:"duration"`
	Source          Source            `json:"source"`
	Metadata        map[string]string `json:"metadata"`
	Statistics      []struct {
		Type                                       string
		Unit                                       string
		Average, Minimum, Maximum, Sum, MostRecent *float64
	} `json:"statistics"`
	Events []struct {
		Type     string            `json:"type"`
		Start    time.Time         `json:"start"`
		End      time.Time         `json:"end"`
		Metadata map[string]string `json:"metadata"`
	} `json:"events"`
	Activities []struct {
		ActivityType string            `json:"activityType"`
		Start        time.Time         `json:"start"`
		End          *time.Time        `json:"end"`
		Metadata     map[string]string `json:"metadata"`
	} `json:"activities"`
}

type RoutePoint struct {
	WorkoutUUID        string    `json:"workoutUUID"`
	RouteUUID          string    `json:"routeUUID"`
	Time               time.Time `json:"time"`
	Latitude           float64   `json:"latitude"`
	Longitude          float64   `json:"longitude"`
	Altitude           float64   `json:"altitude"`
	HorizontalAccuracy float64   `json:"horizontalAccuracy"`
	VerticalAccuracy   float64   `json:"verticalAccuracy"`
	Speed              float64   `json:"speed"`
	Course             float64   `json:"course"`
}

type ActivitySummary struct {
	Date                   string  `json:"date"`
	ActiveEnergyBurned     float64 `json:"activeEnergyBurned"`
	ActiveEnergyBurnedGoal float64 `json:"activeEnergyBurnedGoal"`
	AppleExerciseTime      float64 `json:"appleExerciseTime"`
	AppleExerciseTimeGoal  float64 `json:"appleExerciseTimeGoal"`
	AppleStandHours        float64 `json:"appleStandHours"`
	AppleStandHoursGoal    float64 `json:"appleStandHoursGoal"`
	AppleMoveTime          float64 `json:"appleMoveTime"`
	AppleMoveTimeGoal      float64 `json:"appleMoveTimeGoal"`
}

type Characteristic struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Heartbeat struct {
	SeriesUUID     string  `json:"seriesUUID"`
	TimeSinceStart float64 `json:"timeSinceStart"`
	PrecededByGap  bool    `json:"precededByGap"`
}

type Deleted struct {
	UUID string `json:"uuid"`
	Type string `json:"type"`
}

type TypeDone struct {
	Type    string `json:"type"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

type Done struct {
	TotalSent int `json:"totalSent"`
}

// Message is one decoded line; exactly one field is set. ECG cases are accepted and ignored.
type Message struct {
	Hello           *Hello
	Characteristic  *Characteristic
	Sample          *Sample
	Workout         *Workout
	RoutePoint      *RoutePoint
	Heartbeat       *Heartbeat
	ActivitySummary *ActivitySummary
	Deleted         *Deleted
	TypeDone        *TypeDone
	Done            *Done
	Ignored         string
}

type unlabeled[T any] struct {
	V T `json:"_0"`
}

func decodeMessage(line []byte) (Message, error) {
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(line, &outer); err != nil {
		return Message{}, err
	}
	if len(outer) != 1 {
		return Message{}, fmt.Errorf("expected one key, got %d", len(outer))
	}
	var m Message
	for k, raw := range outer {
		var err error
		switch k {
		case "hello":
			var u unlabeled[Hello]
			err = json.Unmarshal(raw, &u)
			m.Hello = &u.V
		case "characteristic":
			m.Characteristic = new(Characteristic)
			err = json.Unmarshal(raw, m.Characteristic)
		case "sample":
			var u unlabeled[Sample]
			err = json.Unmarshal(raw, &u)
			m.Sample = &u.V
		case "workout":
			var u unlabeled[Workout]
			err = json.Unmarshal(raw, &u)
			m.Workout = &u.V
		case "routePoint":
			var u unlabeled[RoutePoint]
			err = json.Unmarshal(raw, &u)
			m.RoutePoint = &u.V
		case "heartbeat":
			m.Heartbeat = new(Heartbeat)
			err = json.Unmarshal(raw, m.Heartbeat)
		case "activitySummary":
			var u unlabeled[ActivitySummary]
			err = json.Unmarshal(raw, &u)
			m.ActivitySummary = &u.V
		case "deleted":
			m.Deleted = new(Deleted)
			err = json.Unmarshal(raw, m.Deleted)
		case "typeDone":
			m.TypeDone = new(TypeDone)
			err = json.Unmarshal(raw, m.TypeDone)
		case "done":
			m.Done = new(Done)
			err = json.Unmarshal(raw, m.Done)
		case "ecg", "ecgVoltage", "ack", "error":
			m.Ignored = k
		default:
			return Message{}, fmt.Errorf("unknown message %q", k)
		}
		if err != nil {
			return Message{}, fmt.Errorf("%s: %w", k, err)
		}
	}
	return m, nil
}
