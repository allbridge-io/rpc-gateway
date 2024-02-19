package admin

import (
    "encoding/base64"
    "encoding/json"
    "fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/crypto"
	"go.uber.org/zap"
)

const (
    DefaultMaxTokenLifespan = 86400 // Default maximum token lifespan in seconds (24 hours)
)

type TokenPayload struct {
    Iss string `json:"iss"`
    Iat int64  `json:"iat"`
    Sub string `json:"sub"`
}

type TokenPayloadRequest struct {
    Address string `json:"address"`
}

type TokenPayloadResponse struct {
    Payload string `json:"payload"`
}

func AdminAuthGuard(config AdminServerConfig) func(http.Handler) http.Handler {
    return func (next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        authHeader := r.Header.Get("Authorization")
        if authHeader == "" {
            http.Error(w, "Unauthorized: Missing Authorization header", http.StatusUnauthorized)
            return
        }
        payloadBytes, signatureBytes, err := parseAuthorizationHeader(authHeader)
        if err != nil {
            zap.L().Error("Unauthorized", zap.Error(err))
            http.Error(w, "Unauthorized", http.StatusUnauthorized)
            return
        }

        var payload TokenPayload
        err = json.Unmarshal(payloadBytes, &payload)
        if err != nil {
            zap.L().Error("Unauthorized: Invalid Bearer Token Payload", zap.Error(err))
            http.Error(w, "Unauthorized: Invalid Bearer Token Payload", http.StatusUnauthorized)
            return
        }

        err = verifySignature(payload.Sub, payloadBytes, signatureBytes)
        if err != nil {
            zap.L().Error("Unauthorized: Invalid Signature", zap.Error(err))
            http.Error(w, "Unauthorized: Invalid Signature", http.StatusUnauthorized)
            return
        }

        if r.Host != payload.Iss {
            zap.L().Error("Unauthorized: Invalid Issuer", zap.Error(err))
            http.Error(w, "Unauthorized: Invalid Issuer", http.StatusUnauthorized)
            return
        }

        now := time.Now().Unix()
        if payload.Iat > now {
            http.Error(w, "Unauthorized: Token is not yet valid", http.StatusUnauthorized)
            return
        }

        var maxTokenLifespan uint = DefaultMaxTokenLifespan
        if config.MaxTokenLifespan != 0 {
            maxTokenLifespan = config.MaxTokenLifespan
        }
        if now - payload.Iat > int64(maxTokenLifespan) {
            http.Error(w, "Unauthorized: Token expired", http.StatusUnauthorized)
            return
        }

        if !containsStringIgnoreCase(config.Admins, payload.Sub) {
            http.Error(w, "Forbidden", http.StatusForbidden)
            return
        }

        next.ServeHTTP(w, r)
    })
}
}

func GenerateTokenPayload(w http.ResponseWriter, r *http.Request) {
    var requestBody TokenPayloadRequest
    if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
        http.Error(w, "Bad Request", http.StatusBadRequest)
        return
    }

    payload := TokenPayload{
        Iss: r.Host,
        Iat: time.Now().Unix(),
        Sub: requestBody.Address,
    }

    payloadBytes, err := json.Marshal(payload)
    if err != nil {
        http.Error(w, "Failed to marshal JSON", http.StatusInternalServerError)
        return
    }
    encodedPayload := base64.RawURLEncoding.EncodeToString(payloadBytes)

    tokenPayloadResponse := TokenPayloadResponse{
        Payload: encodedPayload,
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(tokenPayloadResponse)
}

func parseAuthorizationHeader(authHeader string) ([]byte, []byte, error) {
	parts := strings.Split(authHeader, " ")
	if len(parts) != 2 || parts[0] != "Bearer" {
		return nil, nil, fmt.Errorf("Invalid Authorization header format")
	}

	bearer := parts[1]
    bearerParts := strings.Split(bearer, ".")
    if len(bearerParts) != 2 {
        return nil, nil, fmt.Errorf("Invalid bearer token format")
    }

    payloadBytes, err := base64.RawURLEncoding.DecodeString(bearerParts[0])
    if err != nil {
        return nil, nil, fmt.Errorf("Failed to decode base64 payload: %v", err)
    }

	signatureBytes, err := base64.RawURLEncoding.DecodeString(bearerParts[1])
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to decode base64 signature: %v", err)
	}

    return payloadBytes, signatureBytes, nil
}

func verifySignature(address string, message []byte, signature []byte) error {
	message = accounts.TextHash(message)

	if signature[crypto.RecoveryIDOffset] == 27 || signature[crypto.RecoveryIDOffset] == 28 {
	    // Transform yellow paper V from 27/28 to 0/1
    	signature[crypto.RecoveryIDOffset] -= 27
    }

	recovered, err := crypto.SigToPub(message, signature)
	if err != nil {
		return fmt.Errorf("failed to recover public key: %v", err)
	}

	recoveredAddr := crypto.PubkeyToAddress(*recovered)
	verified := strings.EqualFold(address, recoveredAddr.Hex())
	if !verified {
		return fmt.Errorf("recovered address does not match claimed address")
	}

	return nil
}

func containsStringIgnoreCase(arr []string, toFind string) bool {
    str := strings.ToLower(toFind)
    for _, s := range arr {
       if strings.ToLower(s) == str {
           return true
       }
    }
    return false
}
