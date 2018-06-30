package main

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"

	"cirello.io/svc/pkg/jwt"
)

func detectedClientCertificate(r *http.Request) *x509.Certificate {
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 && len(r.TLS.PeerCertificates[0].EmailAddresses) > 0 {
		return r.TLS.PeerCertificates[0]
	}
	return nil
}

func stripPort(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	return net.JoinHostPort(host, "443")
}

func handleSSOLogin(svcName string, caPEM []byte, w http.ResponseWriter, r *http.Request) {
	if _, ok := acceptableTargets[r.Host]; !ok {
		log.Println("invalid target:", r.Host)
		http.Error(w, http.StatusText(http.StatusUnauthorized),
			http.StatusUnauthorized)
		return
	}

	switch r.RequestURI {
	case "/ssoLogin":
		if err := r.ParseForm(); err != nil {
			log.Println("cannot read form:", err)
			http.Error(w, http.StatusText(http.StatusUnauthorized),
				http.StatusUnauthorized)
			return
		}

		idToken := url.QueryEscape(r.FormValue("id_token"))
		resp, err := http.Get("https://www.googleapis.com/oauth2/v3/tokeninfo?id_token=" + idToken)
		if err != nil {
			log.Println("cannot validate token:", err)
			http.Error(w, http.StatusText(http.StatusUnauthorized),
				http.StatusUnauthorized)
			return
		}

		var tokenValidation struct {
			AUD   string `json:"aud"`
			Email string `json:"email"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&tokenValidation); err != nil {
			log.Println("cannot parse token validation response:", err)
			http.Error(w, http.StatusText(http.StatusUnauthorized),
				http.StatusUnauthorized)
			return
		}

		if tokenValidation.AUD != googleClientID {
			log.Println("invalid application ID, got:", tokenValidation.AUD)
			http.Error(w, http.StatusText(http.StatusUnauthorized),
				http.StatusUnauthorized)
			return
		}

		rawToken, err := jwt.CreateFromEmail(svcName, caPEM, tokenValidation.Email)
		if err != nil {
			log.Println("cannot parse token validation response:", err)
			http.Error(w, http.StatusText(http.StatusUnauthorized),
				http.StatusUnauthorized)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:  gatewayTokenCookie,
			Value: rawToken,
		})
		fmt.Fprintln(w, tokenValidation.Email)

	default:
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, ssoHTML, googleClientID)
	}
}
