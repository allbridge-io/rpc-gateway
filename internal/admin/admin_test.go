package admin

import (
    "bytes"
    "encoding/base64"
    "encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/0xProject/rpc-gateway/internal/proxy"
)

func createConfig() AdminServerConfig {
    return AdminServerConfig{
		Admins: []string{"0x6Dcbf665293BDDe2237c1A6Af41fd70E969883F0"},
		Domain: "testdomain",
		MaxTokenLifespan: 32000000000,
	}
}

const (
    validAuthToken = "eyJpc3MiOiJ0ZXN0ZG9tYWluIiwiaWF0IjoxNzA4NjA4NDI3LCJzdWIiOiIweDZEY2JmNjY1MjkzQkREZTIyMzdjMUE2QWY0MWZkNzBFOTY5ODgzRjAifQ.eBGUPrvBV-OYEaEU6GK_rGyZDLvpJhg3L_BIApDA-lUVg9u1qAlHj0KjO9TTc9A1hqc9zurqu-flTwaAilZoRxw"
)

type MockTargetManager struct{
    targetConfigs []proxy.TargetConfig
}

func (m *MockTargetManager) GetBlockNumberByName(name string) uint64 {
	return 100500
}

func (m *MockTargetManager) GetTargetConfigs() []proxy.TargetConfig {
    return m.targetConfigs
}

func (m *MockTargetManager) GetTargetConfigByName(name string) *proxy.TargetConfig {
    for _, config := range m.targetConfigs {
		if config.Name == name {
			return &config
		}
	}
	return nil
}

func (m *MockTargetManager) UpdateTargetStatus(targetconfig *proxy.TargetConfig, isDisabled bool) {
    for i, config := range m.targetConfigs {
        if config.Name == targetconfig.Name {
            m.targetConfigs[i].IsDisabled = isDisabled
            break
        }
    }
}

func TestGeneratePayload(t *testing.T) {
    mockTargetManager := &MockTargetManager{}
    server := NewServer(createConfig(), mockTargetManager)

    requestBody := []byte(`{"address":"0x6Dcbf665293BDDe2237c1A6Af41fd70E969883F0"}`)

    req, err := http.NewRequest("POST", "/admin/auth/token", bytes.NewBuffer(requestBody))
    if err != nil {
        t.Fatalf("error creating request: %v", err)
    }

    // execute request
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)

	// assert
	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}
    var responseBody struct {
        Payload string `json:"payload"`
    }
    if err := json.NewDecoder(rr.Body).Decode(&responseBody); err != nil {
        t.Fatalf("failed to decode response body: %v", err)
    }

    payloadBytes, err := base64.RawURLEncoding.DecodeString(responseBody.Payload)
    if err != nil {
        t.Errorf("payload is not a valid base64url string: %v", err)
    }
    if len(payloadBytes) == 0 {
        t.Errorf("decoded payload is empty")
    }
    var payload TokenPayload
    err = json.Unmarshal(payloadBytes, &payload)
    if err != nil {
        t.Errorf("Invalid payload: %v", err)
    }
    expectedSub := "0x6Dcbf665293BDDe2237c1A6Af41fd70E969883F0"
    if payload.Sub != expectedSub {
        t.Errorf("handler returned unexpected Sub in payload: got %v want %v", payload.Sub, expectedSub)
    }
    expectedIss := "testdomain"
    if payload.Iss != expectedIss {
        t.Errorf("handler returned unexpected Iss in payload: got %v want %v", payload.Iss, expectedIss)
    }
}

func TestListTargets(t *testing.T) {
    targetManager := &MockTargetManager{
		targetConfigs: []proxy.TargetConfig{
			{Name: "Server1", IsDisabled: true},
			{Name: "Server2", IsDisabled: false},
		},
	}
	server := NewServer(createConfig(), targetManager)

    req, err := http.NewRequest("GET", "/admin/targets", nil)
    if err != nil {
        t.Fatalf("error creating request: %v", err)
    }
    req.Header.Set("Authorization", "Bearer " + validAuthToken)

    // execute request
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)

	// assert
	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}
	expectedResponseBody := `[{"name":"Server1","disabled":true,"blockNumber":100500},{"name":"Server2","disabled":false,"blockNumber":100500}]`
    if strings.TrimRight(rr.Body.String(), " \n\t") != expectedResponseBody {
        t.Errorf("handler returned unexpected body: got %v want %v", rr.Body.String(), expectedResponseBody)
    }
}

func TestUpdateTargetStatus(t *testing.T) {
    targetManager := &MockTargetManager{
		targetConfigs: []proxy.TargetConfig{
			{Name: "Server1", IsDisabled: true},
			{Name: "Server2", IsDisabled: false},
		},
	}
	server := NewServer(createConfig(), targetManager)

    requestBody := []byte(`{"disabled":true}`)
    req, err := http.NewRequest("POST", "/admin/targets/Server2", bytes.NewBuffer(requestBody))
    if err != nil {
        t.Fatalf("error creating request: %v", err)
    }
    req.Header.Set("Authorization", "Bearer " + validAuthToken)

    // execute request
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)

	// assert
	if status := rr.Code; status != http.StatusNoContent {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusNoContent)
	}
	targetConfig := targetManager.GetTargetConfigByName("Server2")
    if targetConfig.IsDisabled != true {
        t.Errorf("handler hasn't changed target's status: got %v want %v", targetConfig.IsDisabled, true)
    }
}

func TestUpdateTargetStatusWhenNotInAdminList(t *testing.T) {
    targetManager := &MockTargetManager{
		targetConfigs: []proxy.TargetConfig{
			{Name: "Server1", IsDisabled: true},
			{Name: "Server2", IsDisabled: false},
		},
	}
	config := createConfig()
	config.Admins = []string{""}
	server := NewServer(config, targetManager)

    requestBody := []byte(`{"disabled":true}`)
    req, err := http.NewRequest("POST", "/admin/targets/Server2", bytes.NewBuffer(requestBody))
    if err != nil {
        t.Fatalf("error creating request: %v", err)
    }
    req.Header.Set("Authorization", "Bearer " + validAuthToken)

    // execute request
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)

	// assert
	if status := rr.Code; status != http.StatusForbidden {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusNoContent)
	}
	targetConfig := targetManager.GetTargetConfigByName("Server2")
    if targetConfig.IsDisabled != false {
        t.Errorf("handler has changed target's status: got %v want %v", targetConfig.IsDisabled, false)
    }
}
