package security

import (
	"context"
	"errors"
	"fmt"
	"github.com/pbvdven/rdpgw/cmd/rdpgw/identity"
	"github.com/pbvdven/rdpgw/cmd/rdpgw/protocol"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v3"
	"github.com/go-jose/go-jose/v3/jwt"
	"golang.org/x/oauth2"
	"log"
	"time"
)

var (
	SigningKey        []byte
	EncryptionKey     []byte
	UserSigningKey    []byte
	UserEncryptionKey []byte
	QuerySigningKey   []byte
	OIDCProvider      *oidc.Provider
	Oauth2Config      oauth2.Config
)

var ExpiryTime time.Duration = 5
var VerifyClientIP bool = true

type customClaims struct {
	RemoteServer string `json:"remoteServer"`
	ClientIP     string `json:"clientIp"`
	AccessToken  string `json:"accessToken"`
}

func CheckSession(next protocol.CheckHostFunc) protocol.CheckHostFunc {
	return func(ctx context.Context, host string) (bool, error) {
		tunnel := getTunnel(ctx)
		if tunnel == nil {
			return false, errors.New("no valid session info found in context")
		}

		if tunnel.TargetServer != host {
			log.Printf("Client specified host %s does not match token host %s", host, tunnel.TargetServer)
			return false, nil
		}

		// use identity from context rather then set by tunnel
		id := identity.FromCtx(ctx)
		if VerifyClientIP && tunnel.RemoteAddr != id.GetAttribute(identity.AttrClientIp) {
			log.Printf("Current client ip address %s does not match token client ip %s",
				id.GetAttribute(identity.AttrClientIp), tunnel.RemoteAddr)
			return false, nil
		}
		return next(ctx, host)
	}
}

func CheckPAACookie(ctx context.Context, tokenString string) (bool, error) {
	if tokenString == "" {
		log.Printf("no token to parse")
		return false, errors.New("no token to parse")
	}

	token, err := jwt.ParseSigned(tokenString)
	if err != nil {
		log.Printf("cannot parse token due to: %tunnel", err)
		return false, err
	}

	// check if the signing algo matches what we expect
	for _, header := range token.Headers {
		if header.Algorithm != string(jose.HS256) {
			return false, fmt.Errorf("unexpected signing method: %v", header.Algorithm)
		}
	}

	standard := jwt.Claims{}
	custom := customClaims{}

	// Claims automagically checks the signature...
	err = token.Claims(SigningKey, &standard, &custom)
	if err != nil {
		log.Printf("token signature validation failed due to %tunnel", err)
		return false, err
	}

	// ...but doesn't check the expiry claim :/
	err = standard.Validate(jwt.Expected{
		Issuer: "rdpgw",
		Time:   time.Now(),
	})

	if err != nil {
		log.Printf("token validation failed due to %tunnel", err)
		return false, err
	}

	// validate the access token
	tokenSource := Oauth2Config.TokenSource(ctx, &oauth2.Token{AccessToken: custom.AccessToken})
	user, err := OIDCProvider.UserInfo(ctx, tokenSource)
	if err != nil {
		log.Printf("Cannot get user info for access token: %tunnel", err)
		return false, err
	}

	tunnel := getTunnel(ctx)

	tunnel.TargetServer = custom.RemoteServer
	tunnel.RemoteAddr = custom.ClientIP
	tunnel.User.SetUserName(user.Subject)

	return true, nil
}

func GeneratePAAToken(ctx context.Context, username string, server string) (string, error) {
	if len(SigningKey) < 32 {
		return "", errors.New("token signing key not long enough or not specified")
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: SigningKey}, nil)
	if err != nil {
		log.Printf("Cannot obtain signer %s", err)
		return "", err
	}

	standard := jwt.Claims{
		Issuer:  "rdpgw",
		Expiry:  jwt.NewNumericDate(time.Now().Add(time.Minute * 5)),
		Subject: username,
	}

	id := identity.FromCtx(ctx)
	private := customClaims{
		RemoteServer: server,
		ClientIP:     id.GetAttribute(identity.AttrClientIp).(string),
		AccessToken:  id.GetAttribute(identity.AttrAccessToken).(string),
	}

	if token, err := jwt.Signed(sig).Claims(standard).Claims(private).CompactSerialize(); err != nil {
		log.Printf("Cannot sign PAA token %s", err)
		return "", err
	} else {
		return token, nil
	}
}

func GenerateUserToken(ctx context.Context, userName string) (string, error) {
	if len(UserEncryptionKey) < 32 {
		return "", errors.New("user token encryption key not long enough or not specified")
	}

	claims := jwt.Claims{
		Subject: userName,
		Expiry:  jwt.NewNumericDate(time.Now().Add(time.Minute * 5)),
		Issuer:  "rdpgw",
	}

	enc, err := jose.NewEncrypter(
		jose.A128CBC_HS256,
		jose.Recipient{Algorithm: jose.DIRECT, Key: UserEncryptionKey},
		(&jose.EncrypterOptions{Compression: jose.DEFLATE}).WithContentType("JWT"),
	)

	if err != nil {
		log.Printf("Cannot encrypt user token due to %s", err)
		return "", err
	}

	// this makes the token bigger and we deal with a limited space of 511 characters
	// sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: SigningKey}, nil)
	// token, err := jwt.SignedAndEncrypted(sig, enc).Claims(claims).CompactSerialize()
	token, err := jwt.Encrypted(enc).Claims(claims).CompactSerialize()
	return token, err
}

func UserInfo(ctx context.Context, token string) (jwt.Claims, error) {
	standard := jwt.Claims{}
	if len(UserEncryptionKey) > 0 && len(UserSigningKey) > 0 {
		enc, err := jwt.ParseSignedAndEncrypted(token)
		if err != nil {
			log.Printf("Cannot get token %s", err)
			return standard, errors.New("cannot get token")
		}
		token, err := enc.Decrypt(UserEncryptionKey)
		if err != nil {
			log.Printf("Cannot decrypt token %s", err)
			return standard, errors.New("cannot decrypt token")
		}
		if _, err := verifyAlg(token.Headers, string(jose.HS256)); err != nil {
			log.Printf("signature validation failure: %s", err)
			return standard, errors.New("signature validation failure")
		}
		if err = token.Claims(UserSigningKey, &standard); err != nil {
			log.Printf("cannot verify signature %s", err)
			return standard, errors.New("cannot verify signature")
		}
	} else if len(UserSigningKey) == 0 {
		token, err := jwt.ParseEncrypted(token)
		if err != nil {
			log.Printf("Cannot get token %s", err)
			return standard, errors.New("cannot get token")
		}
		err = token.Claims(UserEncryptionKey, &standard)
		if err != nil {
			log.Printf("Cannot decrypt token %s", err)
			return standard, errors.New("cannot decrypt token")
		}
	} else {
		token, err := jwt.ParseSigned(token)
		if err != nil {
			log.Printf("Cannot get token %s", err)
			return standard, errors.New("cannot get token")
		}
		if _, err := verifyAlg(token.Headers, string(jose.HS256)); err != nil {
			log.Printf("signature validation failure: %s", err)
			return standard, errors.New("signature validation failure")
		}
		err = token.Claims(UserSigningKey, &standard)
		if err = token.Claims(UserSigningKey, &standard); err != nil {
			log.Printf("cannot verify signature %s", err)
			return standard, errors.New("cannot verify signature")
		}
	}

	// go-jose doesnt verify the expiry
	err := standard.Validate(jwt.Expected{
		Issuer: "rdpgw",
		Time:   time.Now(),
	})

	if err != nil {
		log.Printf("token validation failed due to %s", err)
		return standard, fmt.Errorf("token validation failed due to %s", err)
	}

	return standard, nil
}

func QueryInfo(ctx context.Context, tokenString string, issuer string) (string, error) {
	standard := jwt.Claims{}
	token, err := jwt.ParseSigned(tokenString)
	if err != nil {
		log.Printf("Cannot get token %s", err)
		return "", errors.New("cannot get token")
	}
	if _, err := verifyAlg(token.Headers, string(jose.HS256)); err != nil {
		log.Printf("signature validation failure: %s", err)
		return "", errors.New("signature validation failure")
	}
	err = token.Claims(QuerySigningKey, &standard)
	if err = token.Claims(QuerySigningKey, &standard); err != nil {
		log.Printf("cannot verify signature %s", err)
		return "", errors.New("cannot verify signature")
	}

	// go-jose doesnt verify the expiry
	err = standard.Validate(jwt.Expected{
		Issuer: issuer,
		Time:   time.Now(),
	})

	if err != nil {
		log.Printf("token validation failed due to %s", err)
		return "", fmt.Errorf("token validation failed due to %s", err)
	}

	return standard.Subject, nil
}

// GenerateQueryToken this is a helper function for testing
func GenerateQueryToken(ctx context.Context, query string, issuer string) (string, error) {
	if len(QuerySigningKey) < 32 {
		return "", errors.New("query token encryption key not long enough or not specified")
	}

	claims := jwt.Claims{
		Subject: query,
		Expiry:  jwt.NewNumericDate(time.Now().Add(time.Minute * 5)),
		Issuer:  issuer,
	}

	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: QuerySigningKey},
		(&jose.SignerOptions{}).WithBase64(true))

	if err != nil {
		log.Printf("Cannot encrypt user token due to %s", err)
		return "", err
	}

	token, err := jwt.Signed(sig).Claims(claims).CompactSerialize()
	return token, err
}

func getTunnel(ctx context.Context) *protocol.Tunnel {
	s, ok := ctx.Value(protocol.CtxTunnel).(*protocol.Tunnel)
	if !ok {
		log.Printf("cannot get session info from context")
		return nil
	}
	return s
}

func verifyAlg(headers []jose.Header, alg string) (bool, error) {
	for _, header := range headers {
		if header.Algorithm != alg {
			return false, fmt.Errorf("invalid signing method %s", header.Algorithm)
		}
	}
	return true, nil
}
