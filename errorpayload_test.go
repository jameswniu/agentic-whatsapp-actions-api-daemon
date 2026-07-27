package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The 2026-07-27 incident: a send failed while actiond's own socket was dead,
// and the caller received only {"error":"whatsapp connection is down"}. That
// string was true about actiond but read as an outage of the OpenClaw whatsapp
// channel, so an operator chased a healthy component twice. These tests pin the
// two properties that prevent a repeat: the message names the component, and
// the real dial failure travels with it verbatim.

func TestNotConnectedNamesTheComponent(t *testing.T) {
	msg := ErrNotConnected.Error()
	if strings.EqualFold(msg, "whatsapp connection is down") {
		t.Fatalf("regressed to the ambiguous pre-incident wording: %q", msg)
	}
	for _, want := range []string{"actiond", "OpenClaw"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ErrNotConnected must disambiguate which session is down; %q lacks %q", msg, want)
		}
	}
}

func TestErrorPayloadCarriesVerbatimCause(t *testing.T) {
	// The literal dial error from logs/actiond.out.log at 2026-07-26T22:56:59.
	const dialErr = `failed to dial whatsapp web websocket: failed to WebSocket dial: ` +
		`failed to send handshake request: Get "https://web.whatsapp.com/ws/chat": ` +
		`dial tcp [2a03:2880:f36e:120:face:b00c:0:167]:443: connect: no route to host`

	p := errorPayload(ErrNotConnected, "send_document", "120363000000000000@g.us", dialErr)

	if got := p["cause"]; got != dialErr {
		t.Errorf("cause must be the dial error verbatim\n got: %v\nwant: %v", got, dialErr)
	}
	if p["error"] != ErrNotConnected.Error() {
		t.Errorf("error must be err.Error() verbatim, got %v", p["error"])
	}
	if p["chat"] != "120363000000000000@g.us" {
		t.Errorf("offending argument must be echoed, got %v", p["chat"])
	}
	if p["action"] != "send_document" {
		t.Errorf("action must be echoed, got %v", p["action"])
	}
	if p["error_type"] != "*errors.errorString" {
		t.Errorf("error_type must be the concrete Go type, got %v", p["error_type"])
	}

	// Must survive the wire: the CLI hands raw stdout to an agent.
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("payload must marshal: %v", err)
	}
	if !strings.Contains(string(b), "no route to host") {
		t.Errorf("serialized payload lost the root cause: %s", b)
	}
}

// A send failure must stay a send failure. Pre-fix, any non-nil error from the
// op reached the caller as a bare string with no type, so a validation error and
// a connectivity error were indistinguishable downstream.
func TestErrorPayloadDoesNotLaunderSendFailureIntoConnectivity(t *testing.T) {
	badArg := errors.New("media (path to document file) is required")
	p := errorPayload(badArg, "send_document", "120363000000000000@g.us", "")

	if _, ok := p["cause"]; ok {
		t.Errorf("no cause is known here; it must be omitted, not guessed: %v", p["cause"])
	}
	if strings.Contains(fmt.Sprint(p["error"]), "connection") {
		t.Errorf("argument error must not mention connectivity: %v", p["error"])
	}
	if p["error"] != badArg.Error() {
		t.Errorf("argument error must propagate verbatim, got %v", p["error"])
	}
}

// A wrapped error surfaces its inner error even when nothing was recorded.
func TestErrorPayloadUnwrapsWhenNoCauseRecorded(t *testing.T) {
	inner := errors.New("upload failed: 413 payload too large")
	p := errorPayload(fmt.Errorf("send_document: %w", inner), "send_document", "x@g.us", "")

	if p["cause"] != inner.Error() {
		t.Errorf("wrapped inner error must surface as cause, got %v", p["cause"])
	}
}
