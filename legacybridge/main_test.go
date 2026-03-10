package main

import (
	"reflect"
	"testing"
)

func TestParseChannelMapOK(t *testing.T) {
	got, err := parseChannelMap("1:0,2:1,5:4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[int]int{1: 0, 2: 1, 5: 4}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseChannelMap mismatch: got=%v want=%v", got, want)
	}
}

func TestParseChannelMapRejectDuplicates(t *testing.T) {
	if _, err := parseChannelMap("1:0,1:2"); err == nil {
		t.Fatalf("expected duplicate user channel error")
	}
	if _, err := parseChannelMap("1:0,2:0"); err == nil {
		t.Fatalf("expected duplicate protocol channel error")
	}
}

func TestParseHubMappingsOffset(t *testing.T) {
	m, err := parseHubMappings("1,2,3", -1, 9100, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m) != 3 {
		t.Fatalf("expected 3 mappings, got %d", len(m))
	}
	if m[0].user != 1 || m[0].proto != 0 || m[0].port != 9101 {
		t.Fatalf("unexpected mapping[0]: %+v", m[0])
	}
	if m[2].user != 3 || m[2].proto != 2 || m[2].port != 9103 {
		t.Fatalf("unexpected mapping[2]: %+v", m[2])
	}
}

func TestParseHubMappingsChannelMapOverride(t *testing.T) {
	chMap := map[int]int{1: 4, 2: 5}
	m, err := parseHubMappings("1,2", -1, 9100, chMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("expected 2 mappings, got %d", len(m))
	}
	if m[0].proto != 4 || m[1].proto != 5 {
		t.Fatalf("unexpected mapped protocol channels: %+v", m)
	}
}

func TestProtocolChannelPriority(t *testing.T) {
	c := &client{cfg: config{
		channel:         3,
		channelBase:     1,
		protocolChannel: -1,
		channelMap:      map[int]int{3: 6},
	}}
	if got := c.protocolChannel(); got != 6 {
		t.Fatalf("expected channel-map to apply, got %d", got)
	}
	c.cfg.protocolChannel = 2
	if got := c.protocolChannel(); got != 2 {
		t.Fatalf("expected explicit protocolChannel override, got %d", got)
	}
}
