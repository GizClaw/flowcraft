package api

import (
	"crypto/rand"
	"encoding/base64"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type JWTConfig struct {
	Secret     []byte
	Expiration time.Duration
}

type Claims struct {
	jwt.RegisteredClaims
	Username string `json:"username"`
}

func (j *JWTConfig) expiration() time.Duration {
	if j.Expiration == 0 {
		return 30 * 24 * time.Hour
	}
	return j.Expiration
}

func (j *JWTConfig) Issue(username string) (string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(j.expiration())
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   username,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(now),
		},
		Username: username,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := token.SignedString(j.Secret)
	if err != nil {
		return "", time.Time{}, err
	}
	return tokenStr, expiresAt, nil
}

func (j *JWTConfig) Validate(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errdefs.Unauthorizedf("unexpected signing method")
		}
		return j.Secret, nil
	})
	if err != nil {
		return nil, errdefs.Unauthorizedf("invalid token: %v", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errdefs.Unauthorizedf("invalid token claims")
	}
	return claims, nil
}

func GenerateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
