package jwt

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"time"

	jwt "github.com/golang-jwt/jwt/v4"
)

type Claims struct {
	Username string `json:"username"`
	ID       int    `json:"id"`
	jwt.RegisteredClaims
}

func GenerateToken(username string, privateKey *ecdsa.PrivateKey, id int, issuer string, t time.Duration) (string, error) {
	claims := Claims{
		username,
		id,
		jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(t)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
			Issuer:    issuer,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	signedToken, err := token.SignedString(privateKey)
	return signedToken, err
}

func ParseToken(signedToken string, publicKeyFunc func() (*ecdsa.PublicKey, error)) (*Claims, error) {
	token, err := jwt.ParseWithClaims(signedToken, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
		}
		return publicKeyFunc()
	})
	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		return claims, nil
	}

	return nil, errors.New("invalid token")
}

const (
	accessIssuer  = "nimoos"
	refreshIssuer = "refresh"
)

// get AccessToken
func GetAccessToken(username string, privateKey *ecdsa.PrivateKey, id int) (string, error) {
	return GenerateToken(username, privateKey, id, accessIssuer, 3*time.Hour)
}

func GetRefreshToken(username string, private *ecdsa.PrivateKey, id int) (string, error) {
	return GenerateToken(username, private, id, refreshIssuer, 7*24*time.Hour)
}

// Validate accepts only access tokens. A refresh token is signed by the same
// key and lives 7 days; letting it through here would turn every refresh
// token into a week-long access token.
func Validate(token string, publicKeyFunc func() (*ecdsa.PublicKey, error)) (bool, *Claims, error) {
	return validateIssuer(token, publicKeyFunc, accessIssuer)
}

// ValidateRefresh accepts only refresh tokens.
func ValidateRefresh(token string, publicKeyFunc func() (*ecdsa.PublicKey, error)) (bool, *Claims, error) {
	return validateIssuer(token, publicKeyFunc, refreshIssuer)
}

func validateIssuer(token string, publicKeyFunc func() (*ecdsa.PublicKey, error), issuer string) (bool, *Claims, error) {
	claims, err := ParseToken(token, publicKeyFunc)
	if err != nil {
		return false, nil, err
	}
	if claims.Issuer != issuer {
		return false, nil, fmt.Errorf("token issuer %q, want %q", claims.Issuer, issuer)
	}
	return true, claims, nil
}
