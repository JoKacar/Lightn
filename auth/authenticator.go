package auth

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"

	"github.com/lightninglabs/kirin/macaroons"
	"github.com/lightninglabs/kirin/mint"
	"github.com/lightninglabs/loop/lsat"
	"github.com/lightningnetwork/lnd/lntypes"
	"gopkg.in/macaroon.v2"
)

const (
	// HeaderAuthorization is the HTTP header field name that is used to
	// send the LSAT by REST clients.
	HeaderAuthorization = "Authorization"

	// HeaderMacaroonMD is the HTTP header field name that is used to send
	// the LSAT by certain REST and gRPC clients.
	HeaderMacaroonMD = "Grpc-Metadata-Macaroon"

	// HeaderMacaroon is the HTTP header field name that is used to send the
	// LSAT by our own gRPC clients.
	HeaderMacaroon = "Macaroon"
)

var (
	authRegex  = regexp.MustCompile("LSAT (.*?):([a-f0-9]{64})")
	authFormat = "LSAT %s:%s"
	opWildcard = "*"
)

// LsatAuthenticator is an authenticator that uses the LSAT protocol to
// authenticate requests.
type LsatAuthenticator struct {
	minter Minter
}

// A compile time flag to ensure the LsatAuthenticator satisfies the
// Authenticator interface.
var _ Authenticator = (*LsatAuthenticator)(nil)

// NewLsatAuthenticator creates a new authenticator that authenticates requests
// based on LSAT tokens.
func NewLsatAuthenticator(minter Minter) *LsatAuthenticator {
	return &LsatAuthenticator{minter: minter}
}

// Accept returns whether or not the header successfully authenticates the user
// to a given backend service.
//
// NOTE: This is part of the Authenticator interface.
func (l *LsatAuthenticator) Accept(header *http.Header, serviceName string) bool {
	// Try reading the macaroon and preimage from the HTTP header. This can
	// be in different header fields depending on the implementation and/or
	// protocol.
	mac, preimage, err := FromHeader(header)
	if err != nil {
		log.Debugf("Deny: %v", err)
		return false
	}

	verificationParams := &mint.VerificationParams{
		Macaroon:      mac,
		Preimage:      preimage,
		TargetService: serviceName,
	}
	err = l.minter.VerifyLSAT(context.Background(), verificationParams)
	if err != nil {
		log.Debugf("Deny: LSAT validation failed: %v", err)
		return false
	}

	return true
}

// FreshChallengeHeader returns a header containing a challenge for the user to
// complete.
//
// NOTE: This is part of the Authenticator interface.
func (l *LsatAuthenticator) FreshChallengeHeader(r *http.Request,
	serviceName string) (http.Header, error) {

	service := lsat.Service{Name: serviceName, Tier: lsat.BaseTier}
	mac, paymentRequest, err := l.minter.MintLSAT(
		context.Background(), service,
	)
	if err != nil {
		log.Errorf("Error minting LSAT: %v", err)
		return nil, err
	}
	macBytes, err := mac.MarshalBinary()
	if err != nil {
		log.Errorf("Error serializing LSAT: %v", err)
	}

	str := fmt.Sprintf("LSAT macaroon='%s' invoice='%s'",
		base64.StdEncoding.EncodeToString(macBytes), paymentRequest)
	header := r.Header
	header.Set("WWW-Authenticate", str)

	log.Debugf("Created new challenge header: [%s]", str)
	return header, nil
}

// FromHeader tries to extract authentication information from HTTP headers.
// There are two supported formats that can be sent in three different header
// fields:
//    1.      Authorization: LSAT <macBase64>:<preimageHex>
//    2.      Grpc-Metadata-Macaroon: <macHex>
//    3.      Macaroon: <macHex>
// If only the macaroon is sent in header 2 or three then it is expected to have
// a caveat with the preimage attached to it.
func FromHeader(header *http.Header) (*macaroon.Macaroon, lntypes.Preimage, error) {
	var authHeader string

	switch {
	// Header field 1 contains the macaroon and the preimage as distinct
	// values separated by a colon.
	case header.Get(HeaderAuthorization) != "":
		// Parse the content of the header field and check that it is in
		// the correct format.
		authHeader = header.Get(HeaderAuthorization)
		log.Debugf("Trying to authorize with header value [%s].",
			authHeader)
		if !authRegex.MatchString(authHeader) {
			return nil, lntypes.Preimage{}, fmt.Errorf("invalid "+
				"auth header format: %s", authHeader)
		}
		matches := authRegex.FindStringSubmatch(authHeader)
		if len(matches) != 3 {
			return nil, lntypes.Preimage{}, fmt.Errorf("invalid "+
				"auth header format: %s", authHeader)
		}

		// Decode the content of the two parts of the header value.
		macBase64, preimageHex := matches[1], matches[2]
		macBytes, err := base64.StdEncoding.DecodeString(macBase64)
		if err != nil {
			return nil, lntypes.Preimage{}, fmt.Errorf("base64 "+
				"decode of macaroon failed: %v", err)
		}
		mac := &macaroon.Macaroon{}
		err = mac.UnmarshalBinary(macBytes)
		if err != nil {
			return nil, lntypes.Preimage{}, fmt.Errorf("unable to "+
				"unmarshal macaroon: %v", err)
		}
		preimage, err := lntypes.MakePreimageFromStr(preimageHex)
		if err != nil {
			return nil, lntypes.Preimage{}, fmt.Errorf("hex "+
				"decode of preimage failed: %v", err)
		}

		// All done, we don't need to extract anything from the
		// macaroon since the preimage was presented separately.
		return mac, preimage, nil

	// Header field 2: Contains only the macaroon.
	case header.Get(HeaderMacaroonMD) != "":
		authHeader = header.Get(HeaderMacaroonMD)

	// Header field 3: Contains only the macaroon.
	case header.Get(HeaderMacaroon) != "":
		authHeader = header.Get(HeaderMacaroon)

	default:
		return nil, lntypes.Preimage{}, fmt.Errorf("no auth header " +
			"provided")
	}

	// For case 2 and 3, we need to actually unmarshal the macaroon to
	// extract the preimage.
	macBytes, err := hex.DecodeString(authHeader)
	if err != nil {
		return nil, lntypes.Preimage{}, fmt.Errorf("hex decode of "+
			"macaroon failed: %v", err)
	}
	mac := &macaroon.Macaroon{}
	err = mac.UnmarshalBinary(macBytes)
	if err != nil {
		return nil, lntypes.Preimage{}, fmt.Errorf("unable to "+
			"unmarshal macaroon: %v", err)
	}
	preimageHex, err := macaroons.ExtractCaveat(mac, macaroons.CondPreimage)
	if err != nil {
		return nil, lntypes.Preimage{}, fmt.Errorf("unable to extract "+
			"preimage from macaroon: %v", err)
	}
	preimage, err := lntypes.MakePreimageFromStr(preimageHex)
	if err != nil {
		return nil, lntypes.Preimage{}, fmt.Errorf("hex decode of "+
			"preimage failed: %v", err)
	}

	return mac, preimage, nil
}

// SetHeader sets the provided authentication elements as the default/standard
// HTTP header for the LSAT protocol.
func SetHeader(header *http.Header, mac *macaroon.Macaroon,
	preimage lntypes.Preimage) error {

	macBytes, err := mac.MarshalBinary()
	if err != nil {
		return err
	}
	value := fmt.Sprintf(
		authFormat, base64.StdEncoding.EncodeToString(macBytes),
		preimage.String(),
	)
	header.Set(HeaderAuthorization, value)
	return nil
}
