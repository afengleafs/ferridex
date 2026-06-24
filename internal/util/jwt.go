package util

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

func ParseJWTClaims(token string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, false
	}
	payload := parts[1]
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		if rem := len(payload) % 4; rem != 0 {
			payload += strings.Repeat("=", 4-rem)
		}
		decoded, err = base64.URLEncoding.DecodeString(payload)
		if err != nil {
			return nil, false
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return nil, false
	}
	return claims, true
}

func JWTExpiry(token string) (time.Time, bool) {
	claims, ok := ParseJWTClaims(token)
	if !ok {
		return time.Time{}, false
	}
	exp, ok := claims["exp"].(float64)
	if !ok || exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(exp), 0), true
}
