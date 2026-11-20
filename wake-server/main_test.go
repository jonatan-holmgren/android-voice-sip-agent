package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakePush struct{ sends atomic.Int32 }

func (f *fakePush) send(token, requestID string, expiresAt int64) error {
	if token == "" || requestID == "" || expiresAt <= time.Now().UnixMilli() {
		return http.ErrAbortHandler
	}
	f.sends.Add(1)
	return nil
}

func TestWakeRequiresAuthenticationAndDeviceRegistration(t *testing.T) {
	s, push := testServer(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/wake", strings.NewReader(`{"device_id":"deviceid-123456789"}`))
	request.Header.Set("Authorization", "Bearer asterisk-secret")
	response := httptest.NewRecorder()
	s.wake(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unregistered device: got %d, want %d", response.Code, http.StatusNotFound)
	}
	if push.sends.Load() != 0 {
		t.Fatal("sent FCM to an unregistered device")
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/wake", strings.NewReader(`{"device_id":"deviceid-123456789"}`))
	response = httptest.NewRecorder()
	s.wake(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated wake: got %d", response.Code)
	}
}

func TestAuthenticatedWakeSendsOnePushForIdempotentRetry(t *testing.T) {
	s, push := testServer(t)
	if err := s.store.put("deviceid-123456789", strings.Repeat("x", 64)); err != nil {
		t.Fatal(err)
	}
	var first wakeResponse
	for i := 0; i < 2; i++ {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/wake", strings.NewReader(`{"device_id":"deviceid-123456789"}`))
		request.Header.Set("Authorization", "Bearer asterisk-secret")
		request.Header.Set("Idempotency-Key", "call.unique.1")
		response := httptest.NewRecorder()
		s.wake(response, request)
		if response.Code != http.StatusAccepted {
			t.Fatalf("wake attempt %d: got HTTP %d: %s", i, response.Code, response.Body.String())
		}
		var got wakeResponse
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = got
		} else if got.RequestID != first.RequestID || got.ExpiresAt != first.ExpiresAt {
			t.Fatalf("idempotent response changed: %+v then %+v", first, got)
		}
	}
	if push.sends.Load() != 1 {
		t.Fatalf("FCM sends = %d, want 1", push.sends.Load())
	}
}

func TestDeviceStorePersistsWithPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "devices.json")
	store, err := openDeviceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.put("deviceid-123456789", strings.Repeat("t", 64)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("device store mode = %04o, want 0600", got)
	}
}

func TestWakeMessageContainsOnlyAvailabilityEnvelope(t *testing.T) {
	payload, err := json.Marshal(makeWakePayload("fcm-token", "wake-request", 123456))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]map[string]any
	if err = json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	message := decoded["message"]
	data := message["data"].(map[string]any)
	if len(data) != 3 || data["type"] != "gateway_wake" || data["request_id"] != "wake-request" {
		t.Fatalf("unexpected wake data fields: %#v", data)
	}
	if _, ok := message["notification"]; ok {
		t.Fatal("wake payload must remain data-only")
	}
	if strings.Contains(string(payload), "phone_number") || strings.Contains(string(payload), "password") {
		t.Fatal("wake payload contains call or credential data")
	}
}

func TestReadinessRequiresPhoneReportAndMatchesRegisteredDevice(t *testing.T) {
	s, _ := testServer(t)
	expires := time.Now().Add(time.Minute).UnixMilli()
	s.rememberWake("wake-request-123456", "deviceid-123456789", expires)

	request := httptest.NewRequest(http.MethodPost, "/api/v1/wake/wake-request-123456/ready",
		strings.NewReader(`{"installation_id":"other-device-123456","sip_registered":true,"cellular_ready":true,"shizuku_ready":true}`))
	request.SetPathValue("requestID", "wake-request-123456")
	request.Header.Set("Authorization", "Bearer registration-secret")
	response := httptest.NewRecorder()
	s.reportReady(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("wrong device readiness: got HTTP %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/wake/wake-request-123456/ready",
		strings.NewReader(`{"installation_id":"deviceid-123456789","sip_registered":true,"cellular_ready":true,"shizuku_ready":true}`))
	request.SetPathValue("requestID", "wake-request-123456")
	request.Header.Set("Authorization", "Bearer registration-secret")
	response = httptest.NewRecorder()
	s.reportReady(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("valid readiness: got HTTP %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/wake/wake-request-123456", nil)
	request.SetPathValue("requestID", "wake-request-123456")
	request.Header.Set("Authorization", "Bearer asterisk-secret")
	response = httptest.NewRecorder()
	s.wakeStatus(response, request)
	var status readinessResponse
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || status.State != "ready" {
		t.Fatalf("wake status = HTTP %d %+v", response.Code, status)
	}
}

func testServer(t *testing.T) (*server, *fakePush) {
	t.Helper()
	store, err := openDeviceStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	push := &fakePush{}
	return &server{store: store, firebase: push, registrationKey: "registration-secret",
		asteriskKey: "asterisk-secret", idem: map[string]idempotentResult{},
		idemPending: map[string]chan struct{}{}}, push
}
