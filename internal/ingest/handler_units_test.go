package ingest

import (
	"encoding/json"
	"testing"
)

// ── parseUnitEventData ───────────────────────────────────────────────

func TestParseUnitEventData(t *testing.T) {
	t.Run("join_event", func(t *testing.T) {
		payload := []byte(`{
			"type": "unit_event",
			"timestamp": 1700000000,
			"instance_id": "tr-1",
			"join": {
				"sys_num": 0,
				"sys_name": "butco",
				"unit": 12345,
				"unit_alpha_tag": "Engine 1",
				"talkgroup": 100,
				"talkgroup_alpha_tag": "Fire Dispatch"
			}
		}`)
		env, data, err := parseUnitEventData(payload, "join")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if env.InstanceID != "tr-1" {
			t.Errorf("InstanceID = %q, want %q", env.InstanceID, "tr-1")
		}
		if env.Timestamp != 1700000000 {
			t.Errorf("Timestamp = %d, want 1700000000", env.Timestamp)
		}
		if data.Unit != 12345 {
			t.Errorf("Unit = %d, want 12345", data.Unit)
		}
		if data.SysName != "butco" {
			t.Errorf("SysName = %q, want %q", data.SysName, "butco")
		}
		if data.Talkgroup != 100 {
			t.Errorf("Talkgroup = %d, want 100", data.Talkgroup)
		}
		if data.TalkgroupAlphaTag != "Fire Dispatch" {
			t.Errorf("TalkgroupAlphaTag = %q, want %q", data.TalkgroupAlphaTag, "Fire Dispatch")
		}
	})

	t.Run("off_event", func(t *testing.T) {
		payload := []byte(`{
			"type": "unit_event",
			"timestamp": 1700000001,
			"instance_id": "tr-2",
			"off": {
				"sys_name": "warco",
				"unit": 54321
			}
		}`)
		_, data, err := parseUnitEventData(payload, "off")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if data.Unit != 54321 {
			t.Errorf("Unit = %d, want 54321", data.Unit)
		}
	})

	t.Run("call_event_with_position", func(t *testing.T) {
		payload := []byte(`{
			"type": "unit_event",
			"timestamp": 1700000002,
			"instance_id": "tr-1",
			"call": {
				"sys_name": "butco",
				"unit": 100,
				"talkgroup": 200,
				"position": 1.5,
				"length": 3.2,
				"emergency": true
			}
		}`)
		_, data, err := parseUnitEventData(payload, "call")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if data.Position != 1.5 {
			t.Errorf("Position = %f, want 1.5", data.Position)
		}
		if data.Length != 3.2 {
			t.Errorf("Length = %f, want 3.2", data.Length)
		}
		if !data.Emergency {
			t.Error("Emergency should be true")
		}
	})

	t.Run("missing_event_type_key", func(t *testing.T) {
		payload := []byte(`{
			"type": "unit_event",
			"timestamp": 1700000003,
			"instance_id": "tr-1",
			"join": {"unit": 100}
		}`)
		_, _, err := parseUnitEventData(payload, "off")
		if err == nil {
			t.Error("expected error for missing event type key")
		}
	})

	t.Run("invalid_json", func(t *testing.T) {
		_, _, err := parseUnitEventData([]byte(`{not json`), "join")
		if err == nil {
			t.Error("expected error for invalid JSON")
		}
	})
}

// ── round-trip: route parsing + payload parsing ─────────────────────

func TestUnitEventParseRoundTrip(t *testing.T) {
	topic := "trengine/units/butco/join"
	payload, _ := json.Marshal(map[string]any{
		"type":        "unit_event",
		"timestamp":   1700000000,
		"instance_id": "tr-1",
		"join": map[string]any{
			"sys_name":  "butco",
			"unit":      12345,
			"talkgroup": 100,
		},
	})

	route := ParseTopic(topic)
	if route == nil {
		t.Fatal("ParseTopic returned nil")
	}
	if route.Handler != "unit_event" {
		t.Fatalf("Handler = %q, want %q", route.Handler, "unit_event")
	}
	if route.EventType != "join" {
		t.Fatalf("EventType = %q, want %q", route.EventType, "join")
	}

	env, data, err := parseUnitEventData(payload, route.EventType)
	if err != nil {
		t.Fatalf("parseUnitEventData: %v", err)
	}
	if env.InstanceID != "tr-1" {
		t.Errorf("InstanceID = %q, want %q", env.InstanceID, "tr-1")
	}
	if data.Unit != 12345 {
		t.Errorf("Unit = %d, want 12345", data.Unit)
	}
	if data.Talkgroup != 100 {
		t.Errorf("Talkgroup = %d, want 100", data.Talkgroup)
	}
}
