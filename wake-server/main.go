package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const wakeTTL = 90 * time.Second

var (
	deviceIDPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{16,96}$`)
	idempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

type device struct {
	Token string `json:"token"`
}

type deviceStore struct {
	mu      sync.Mutex
	path    string
	devices map[string]device
}

func openDeviceStore(path string) (*deviceStore, error) {
	s := &deviceStore{path: path, devices: map[string]device{}}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal(data, &s.devices); err != nil {
			return nil, fmt.Errorf("invalid device store")
		}
	}
	return s, nil
}

func (s *deviceStore) get(id string) (device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	return d, ok
}

func (s *deviceStore) put(id, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices[id] = device{Token: token}
	return s.persist()
}

func (s *deviceStore) delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.devices, id)
	return s.persist()
}

func (s *deviceStore) persist() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(s.devices)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".vsa-devices-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.path)
}

type limiter struct {
	mu     sync.Mutex
	window map[string]rateWindow
}

type rateWindow struct {
	started time.Time
	count   int
}

func (l *limiter) allow(key string, now time.Time, max int, span time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.window == nil {
		l.window = map[string]rateWindow{}
	}
	w := l.window[key]
	if now.Sub(w.started) >= span {
		w = rateWindow{started: now}
	}
	if w.count >= max {
		l.window[key] = w
		return false
	}
	w.count++
	l.window[key] = w
	return true
}

type idempotentResult struct {
	requestID string
	expiresAt int64
	createdAt time.Time
}

type server struct {
	store           *deviceStore
	firebase        pushSender
	registrationKey string
	asteriskKey     string
	limiter         limiter
	idemMu          sync.Mutex
	idem            map[string]idempotentResult
	idemPending     map[string]chan struct{}
	wakeMu          sync.Mutex
	wakes           map[string]wakeState
}

type pushSender interface {
	send(token, requestID string, expiresAt int64) error
}

type tokenRequest struct {
	InstallationID string `json:"installation_id"`
	Token          string `json:"token"`
}

type wakeRequest struct {
	DeviceID string `json:"device_id"`
}

type wakeResponse struct {
	Accepted  bool   `json:"accepted"`
	RequestID string `json:"request_id"`
	ExpiresAt int64  `json:"expires_at_ms"`
}

type wakeState struct {
	DeviceID string
	Expires  int64
	Ready    bool
}

type readinessReport struct {
	InstallationID string `json:"installation_id"`
	SIPRegistered  bool   `json:"sip_registered"`
	CellularReady  bool   `json:"cellular_ready"`
	ShizukuReady   bool   `json:"shizuku_ready"`
}

type readinessResponse struct {
	State     string `json:"state"`
	ExpiresAt int64  `json:"expires_at_ms"`
}

func main() {
	storePath := env("VSA_DEVICE_STORE", "/var/lib/vsa-wake/devices.json")
	store, err := openDeviceStore(storePath)
	if err != nil {
		log.Fatal("cannot open device store")
	}
	fb, err := newFirebaseClient(env("GOOGLE_APPLICATION_CREDENTIALS", ""))
	if err != nil {
		log.Fatalf("Firebase configuration error: %v", err)
	}
	registrationKey := os.Getenv("VSA_DEVICE_REGISTRATION_KEY")
	asteriskKey := os.Getenv("VSA_ASTERISK_API_KEY")
	if registrationKey == "" || asteriskKey == "" {
		log.Fatal("VSA_DEVICE_REGISTRATION_KEY and VSA_ASTERISK_API_KEY are required")
	}
	s := &server{store: store, firebase: fb, registrationKey: registrationKey, asteriskKey: asteriskKey,
		idem: map[string]idempotentResult{}, idemPending: map[string]chan struct{}{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("PUT /api/v1/devices/token", s.registerToken)
	mux.HandleFunc("POST /api/v1/wake", s.wake)
	mux.HandleFunc("GET /api/v1/wake/{requestID}", s.wakeStatus)
	mux.HandleFunc("POST /api/v1/wake/{requestID}/ready", s.reportReady)
	addr := env("VSA_LISTEN_ADDR", "127.0.0.1:8077")
	log.Printf("wake API listening on %s", addr)
	service := &http.Server{Addr: addr, Handler: securityHeaders(mux), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	log.Fatal(service.ListenAndServe())
}

func (s *server) registerToken(w http.ResponseWriter, r *http.Request) {
	if !bearerEqual(r, s.registrationKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.limiter.allow("register:"+r.RemoteAddr, time.Now(), 20, time.Minute) {
		http.Error(w, "rate limit", http.StatusTooManyRequests)
		return
	}
	var req tokenRequest
	if !decodeJSON(w, r, &req) || !deviceIDPattern.MatchString(req.InstallationID) || len(req.Token) < 40 || len(req.Token) > 4096 {
		http.Error(w, "invalid registration", http.StatusBadRequest)
		return
	}
	if err := s.store.put(req.InstallationID, req.Token); err != nil {
		log.Printf("token persistence failed")
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) wake(w http.ResponseWriter, r *http.Request) {
	if !bearerEqual(r, s.asteriskKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req wakeRequest
	if !decodeJSON(w, r, &req) || !deviceIDPattern.MatchString(req.DeviceID) {
		http.Error(w, "invalid wake request", http.StatusBadRequest)
		return
	}
	if !s.limiter.allow("wake:"+req.DeviceID, time.Now(), 6, time.Minute) {
		http.Error(w, "rate limit", http.StatusTooManyRequests)
		return
	}
	if _, ok := s.store.get(req.DeviceID); !ok {
		http.Error(w, "device not registered", http.StatusNotFound)
		return
	}
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey != "" {
		if !idempotencyPattern.MatchString(idemKey) {
			http.Error(w, "invalid idempotency key", http.StatusBadRequest)
			return
		}
		if previous, ok := s.beginIdempotent(req.DeviceID+":"+idemKey, time.Now()); ok {
			writeJSON(w, http.StatusAccepted, wakeResponse{Accepted: true, RequestID: previous.requestID, ExpiresAt: previous.expiresAt})
			return
		}
		defer s.finishIdempotent(req.DeviceID+":"+idemKey, nil)
	}
	requestID, err := randomID()
	if err != nil {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	expires := time.Now().Add(wakeTTL).UnixMilli()
	s.rememberWake(requestID, req.DeviceID, expires)
	d, _ := s.store.get(req.DeviceID)
	if err = s.firebase.send(d.Token, requestID, expires); err != nil {
		s.forgetWake(requestID)
		if errors.Is(err, errUnregistered) {
			_ = s.store.delete(req.DeviceID)
		}
		log.Printf("FCM wake send failed")
		http.Error(w, "push unavailable", http.StatusBadGateway)
		return
	}
	if idemKey != "" {
		s.finishIdempotent(req.DeviceID+":"+idemKey, &idempotentResult{requestID: requestID, expiresAt: expires, createdAt: time.Now()})
	}
	writeJSON(w, http.StatusAccepted, wakeResponse{Accepted: true, RequestID: requestID, ExpiresAt: expires})
}

func (s *server) wakeStatus(w http.ResponseWriter, r *http.Request) {
	if !bearerEqual(r, s.asteriskKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	requestID := r.PathValue("requestID")
	if !deviceIDPattern.MatchString(requestID) {
		http.Error(w, "unknown wake request", http.StatusNotFound)
		return
	}
	wake, ok := s.getWake(requestID)
	if !ok {
		http.Error(w, "unknown wake request", http.StatusNotFound)
		return
	}
	state := "pending"
	if time.Now().UnixMilli() >= wake.Expires {
		state = "failed"
	} else if wake.Ready {
		state = "ready"
	}
	writeJSON(w, http.StatusOK, readinessResponse{State: state, ExpiresAt: wake.Expires})
}

func (s *server) reportReady(w http.ResponseWriter, r *http.Request) {
	if !bearerEqual(r, s.registrationKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	requestID := r.PathValue("requestID")
	if !deviceIDPattern.MatchString(requestID) {
		http.Error(w, "unknown wake request", http.StatusNotFound)
		return
	}
	var report readinessReport
	if !decodeJSON(w, r, &report) || !report.SIPRegistered || !report.CellularReady || !report.ShizukuReady {
		http.Error(w, "gateway is not ready", http.StatusConflict)
		return
	}
	s.wakeMu.Lock()
	wake, ok := s.wakes[requestID]
	if !ok || wake.DeviceID != report.InstallationID || time.Now().UnixMilli() >= wake.Expires {
		s.wakeMu.Unlock()
		http.Error(w, "unknown or expired wake request", http.StatusNotFound)
		return
	}
	wake.Ready = true
	s.wakes[requestID] = wake
	s.wakeMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) rememberWake(id, deviceID string, expires int64) {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	now := time.Now().UnixMilli()
	for key, value := range s.wakes {
		if now >= value.Expires {
			delete(s.wakes, key)
		}
	}
	if s.wakes == nil {
		s.wakes = map[string]wakeState{}
	}
	s.wakes[id] = wakeState{DeviceID: deviceID, Expires: expires}
}

func (s *server) getWake(id string) (wakeState, bool) {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	wake, ok := s.wakes[id]
	return wake, ok
}

func (s *server) forgetWake(id string) {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	delete(s.wakes, id)
}

func (s *server) beginIdempotent(key string, now time.Time) (idempotentResult, bool) {
	for {
		s.idemMu.Lock()
		for k, v := range s.idem {
			if now.Sub(v.createdAt) > 2*time.Minute {
				delete(s.idem, k)
			}
		}
		if result, ok := s.idem[key]; ok {
			s.idemMu.Unlock()
			return result, true
		}
		if pending := s.idemPending[key]; pending != nil {
			s.idemMu.Unlock()
			<-pending
			continue
		}
		s.idemPending[key] = make(chan struct{})
		s.idemMu.Unlock()
		return idempotentResult{}, false
	}
}

func (s *server) finishIdempotent(key string, result *idempotentResult) {
	s.idemMu.Lock()
	defer s.idemMu.Unlock()
	if result != nil {
		s.idem[key] = *result
	}
	if pending := s.idemPending[key]; pending != nil {
		delete(s.idemPending, key)
		close(pending)
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(out) == nil && decoder.Decode(new(any)) == io.EOF
}

func bearerEqual(r *http.Request, expected string) bool {
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return len(value) == len(expected) && subtle.ConstantTimeCompare([]byte(value), []byte(expected)) == 1
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func randomID() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

type firebaseAccount struct {
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

type firebaseClient struct {
	account firebaseAccount
	key     *rsa.PrivateKey
	http    *http.Client
	mu      sync.Mutex
	token   string
	tokenTo time.Time
}

var errUnregistered = errors.New("unregistered FCM token")

func newFirebaseClient(path string) (*firebaseClient, error) {
	if path == "" {
		return nil, errors.New("GOOGLE_APPLICATION_CREDENTIALS is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read service-account file")
	}
	var account firebaseAccount
	if err = json.Unmarshal(data, &account); err != nil {
		return nil, errors.New("invalid service-account JSON")
	}
	block, _ := pem.Decode([]byte(account.PrivateKey))
	if block == nil {
		return nil, errors.New("service-account private key is invalid")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("service-account private key is invalid")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok || account.ProjectID == "" || account.ClientEmail == "" {
		return nil, errors.New("service-account fields are incomplete")
	}
	if account.TokenURI == "" {
		account.TokenURI = "https://oauth2.googleapis.com/token"
	}
	if !strings.HasPrefix(account.TokenURI, "https://") {
		return nil, errors.New("OAuth token URI must use HTTPS")
	}
	return &firebaseClient{account: account, key: key, http: &http.Client{Timeout: 8 * time.Second}}, nil
}

func (f *firebaseClient) send(token, requestID string, expiresAt int64) error {
	oauth, err := f.accessToken()
	if err != nil {
		return err
	}
	body, err := json.Marshal(makeWakePayload(token, requestID, expiresAt))
	if err != nil {
		return err
	}
	endpoint := "https://fcm.googleapis.com/v1/projects/" + f.account.ProjectID + "/messages:send"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+oauth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity {
		return errUnregistered
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("FCM HTTP %d", resp.StatusCode)
	}
	return nil
}

func makeWakePayload(token, requestID string, expiresAt int64) map[string]any {
	return map[string]any{"message": map[string]any{
		"token":   token,
		"data":    map[string]string{"type": "gateway_wake", "request_id": requestID, "expires_at_ms": fmt.Sprint(expiresAt)},
		"android": map[string]any{"priority": "HIGH", "ttl": "90s"},
	}}
}

func (f *firebaseClient) accessToken() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if time.Until(f.tokenTo) > time.Minute {
		return f.token, nil
	}
	now := time.Now()
	claims := map[string]any{"iss": f.account.ClientEmail, "scope": "https://www.googleapis.com/auth/firebase.messaging",
		"aud": f.account.TokenURI, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	unsigned, err := jwtUnsigned(claims)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	assertion := unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
	form := "grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Ajwt-bearer&assertion=" + assertion
	req, err := http.NewRequest(http.MethodPost, f.account.TokenURI, strings.NewReader(form))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Google OAuth HTTP %d", resp.StatusCode)
	}
	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 16*1024)).Decode(&result); err != nil || result.AccessToken == "" {
		return "", errors.New("invalid Google OAuth response")
	}
	f.token = result.AccessToken
	f.tokenTo = now.Add(time.Duration(result.ExpiresIn) * time.Second)
	return f.token, nil
}

func jwtUnsigned(claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload), nil
}
