package vault

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"hash"
	"net/url"

	"github.com/hashicorp/go-secure-stdlib/strutil"
	"gopkg.in/square/go-jose.v2"
)

// validRedirect checks whether uri is in allowed using special handling for loopback uris.
// Ref: https://tools.ietf.org/html/rfc8252#section-7.3
func validRedirect(uri string, allowed []string) bool {
	inputURI, err := url.Parse(uri)
	if err != nil {
		return false
	}

	// if uri isn't a loopback, just string search the allowed list
	if !strutil.StrListContains([]string{"localhost", "127.0.0.1", "::1"}, inputURI.Hostname()) {
		return strutil.StrListContains(allowed, uri)
	}

	// otherwise, search for a match in a port-agnostic manner, per the OAuth RFC.
	inputURI.Host = inputURI.Hostname()

	for _, a := range allowed {
		allowedURI, err := url.Parse(a)
		if err != nil {
			return false
		}
		allowedURI.Host = allowedURI.Hostname()

		if inputURI.String() == allowedURI.String() {
			return true
		}
	}

	return false
}

// computeHashClaim computes the hash value to be used for the at_hash
// and c_hash claims. For details on how this value is computed and the
// class of attacks it's used to prevent, see the spec at
// - https://openid.net/specs/openid-connect-core-1_0.html#CodeIDToken
// - https://openid.net/specs/openid-connect-core-1_0.html#HybridIDToken
// - https://openid.net/specs/openid-connect-core-1_0.html#TokenSubstitution
func computeHashClaim(alg string, input string) (string, error) {
	signatureAlgToHash := map[jose.SignatureAlgorithm]func() hash.Hash{
		jose.RS256: sha256.New,
		jose.RS384: sha512.New384,
		jose.RS512: sha512.New,
		jose.ES256: sha256.New,
		jose.ES384: sha512.New384,
		jose.ES512: sha512.New,

		// We use the Ed25519 curve key for EdDSA, which uses
		// SHA-512 for its digest algorithm. See details at
		// https://bitbucket.org/openid/connect/issues/1125.
		jose.EdDSA: sha512.New,
	}

	newHash, ok := signatureAlgToHash[jose.SignatureAlgorithm(alg)]
	if !ok {
		return "", fmt.Errorf("unsupported signature algorithm: %q", alg)
	}
	h := newHash()

	// Writing to the hash will never return an error
	_, _ = h.Write([]byte(input))
	sum := h.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(sum[:len(sum)/2]), nil
}
