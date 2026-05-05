package auth

import (
	"github.com/pquerna/otp/totp"
)

// GenerateTOTP creates a fresh TOTP secret keyed for the given account.
// Returns the otpauth URL (for QR) and the base32 secret.
func GenerateTOTP(issuer, account string) (otpauth, secret string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: account,
	})
	if err != nil {
		return "", "", err
	}
	return key.URL(), key.Secret(), nil
}

// VerifyTOTP checks a 6-digit code against a base32 secret.
func VerifyTOTP(secret, code string) bool {
	if secret == "" {
		return true // 2FA not configured
	}
	return totp.Validate(code, secret)
}
