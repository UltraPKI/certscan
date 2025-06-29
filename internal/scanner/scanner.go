// Package scanner provides network scanning and certificate discovery logic for UltraPKI.
// It supports scanning IPs and hostnames for TLS-enabled services, extracting certificates,
// and sending results to a webhook endpoint. Protocol-specific logic (e.g., SMTP STARTTLS)
// is modularized for maintainability.
package scanner

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nextpki/certscan/internal/config"
	"github.com/nextpki/certscan/internal/logutil"
	"github.com/nextpki/certscan/internal/shared"
	utls "github.com/refraction-networking/utls"
)

// AllowedProtocols lists all supported protocol names for scanning.
// Used to validate and dispatch protocol-specific handlers.
var AllowedProtocols = []string{"http1", "h2", "h3", "smtp", "ldap", "imap", "pop3", "custom"}

// Payload represents the data sent to the webhook, including agent and scan results.
// Contains the primary IP, machine ID, and a list of scan results.
type Payload struct {
	PrimaryIP   string       `json:"primary_ip,omitempty"` // The primary IP address of the scanning agent
	MachineID   string       `json:"machine_id,omitempty"` // Unique machine identifier
	ScanResults []ScanResult `json:"scan_results"`         // List of scan results
}

// ScanResult holds the result of a single port scan, including certificates and metadata.
// Used for reporting to the webhook.
type ScanResult struct {
	IP            string   `json:"ip"`                       // Target IP address
	Port          int      `json:"port"`                     // Target port
	Hostname      string   `json:"hostname,omitempty"`       // Optional: original hostname
	HandshakeType string   `json:"handshake_type,omitempty"` // TLS handshake type (ecdsa/rsa)
	Certificates  []string `json:"certificates,omitempty"`   // Base64-encoded DER certificates
	Timestamp     int64    `json:"timestamp"`                // Unix timestamp of scan
}

// matchWildcard checks if s matches pattern (supports '*' wildcard)
func matchWildcard(pattern, s string) bool {
	if pattern == "" {
		return false
	}
	pattern = strings.ToLower(pattern)
	s = strings.ToLower(s)
	if pattern == "*" {
		return true
	}
	if strings.HasPrefix(pattern, "*") && strings.HasSuffix(pattern, "*") {
		return strings.Contains(s, pattern[1:len(pattern)-1])
	}
	if strings.HasPrefix(pattern, "*") {
		return strings.HasSuffix(s, pattern[1:])
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(s, pattern[:len(pattern)-1])
	}
	return s == pattern
}

// isCertExcluded checks if a certificate matches any exclude_certs rule
func isCertExcluded(cert *x509.Certificate, rules []config.ExcludeCertRule) bool {
	issuer := cert.Issuer.String()
	cn := cert.Subject.CommonName
	for _, rule := range rules {
		if (rule.Issuer != "" && matchWildcard(rule.Issuer, issuer)) ||
			(rule.CN != "" && matchWildcard(rule.CN, cn)) {
			return true
		}
	}
	return false
}

// filterCerts filters out excluded certs and returns a slice of base64 DER strings
func filterCerts(derCerts [][]byte, rules []config.ExcludeCertRule) []string {
	var filtered []string
	for _, der := range derCerts {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			continue // skip invalid certs
		}
		if isCertExcluded(cert, rules) {
			logutil.DebugLog("Skipping certificate due to exclude_certs filter: CN=%s Issuer=%s", cert.Subject.CommonName, cert.Issuer.String())
			continue
		}
		filtered = append(filtered, base64.StdEncoding.EncodeToString(der))
	}
	return filtered
}

// sendToWebhook posts scan results to the configured webhook URL as a JSON payload.
// Adds authentication headers if configured. Handles error reporting and token validation.
// Parameters:
//
//	results: List of ScanResult to send
//	url:     Webhook endpoint
func sendToWebhook(results []ScanResult, url string) {
	webhookTimeout := time.Duration(shared.Config.WebhookTimeoutMs)
	if webhookTimeout <= 0 {
		webhookTimeout = 5000
	}
	webhookTimeout = webhookTimeout * time.Millisecond

	payload := Payload{
		PrimaryIP:   shared.GetPrimaryIP(),
		MachineID:   shared.GetMachineID(),
		ScanResults: results,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		logutil.ErrorLog("Failed to marshal results: %v", err)
		return
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		logutil.ErrorLog("Failed to create request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	if shared.Config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+shared.Config.Token)
		req.Header.Set("x-ultrapki-machine-id", shared.GetMachineID())
	}

	client := &http.Client{Timeout: webhookTimeout}
	resp, err := client.Do(req)
	if err != nil {
		logutil.ErrorLog("Webhook request failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logutil.ErrorLog("Webhook returned status: %d", resp.StatusCode)
		// If 403, it might be an invalid token
		if resp.StatusCode == http.StatusForbidden {
			logutil.ErrorLog("Invalid or missing token for webhook %s", url)
			// Quit the program if token is invalid and ask user to
			// go to https://ultrapki.com/ to get instructions to
			// get a new token
			if shared.Config == nil || shared.Config.Token == "" {
				fmt.Println("\n\nNo token provided.")
				fmt.Println("You can register your system in seconds with the following command:\n")
				fmt.Println("  curl -sSf https://cd.ultrapki.com/sh | sh")
				fmt.Println("\nThis will generate a token for your system and show you how to add it to your config.\n")
				os.Exit(1)
			}
		}
	}
}

// ResolveAndScan resolves a hostname (or IP string) and scans each resolved IP.
// Skips IPv6 addresses if not enabled in config. Used for hostnames and CIDR expansion.
// Parameters:
//
//	host:  Hostname or IP string
//	ports: List of ports to scan
func ResolveAndScan(host string, ports []int) {
	ip := net.ParseIP(host)
	if ip != nil {

		logutil.DebugLog("Scanning resolved IP: %s", ip.String())
		ScanAndSendWithProtocol(ip.String(), host, ports, "http1")
		return
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		logutil.ErrorLog("Could not resolve %s: %v", host, err)
		return
	}

	logutil.DebugLog("Resolved %s → %v", host, ips)
	for _, ip := range ips {
		// Debug output for each resolved IP
		logutil.DebugLog("Scanning resolved IP: %s", ip.String())
		ScanAndSendWithProtocol(ip.String(), host, ports, "http1")
	}
}

// tlsHandshakeAndCollectWithTimeout performs a TLS handshake with the given cipher suites and collects certificates.
// Uses utls for full ClientHello customization. Skips HTTP header collection for utls.UConn.
// Parameters:
//
//	ip:            Target IP address
//	hostname:      SNI/Host header
//	port:          Target port
//	suites:        Cipher suites to offer
//	handshakeType: "ecdsa" or "rsa"
//	proto:         Protocol string (e.g., "http1")
//	dialTimeout:   Timeout for TCP dial
//
// Returns: ScanResult or error
func tlsHandshakeAndCollectWithTimeout(ip, hostname string, port int, suites []uint16, handshakeType string, proto string, dialTimeout time.Duration) (*ScanResult, error) {
	address := fmt.Sprintf("%s:%d", ip, port)
	dialer := &net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.Dial("tcp", address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	var signatureAlgs []utls.SignatureScheme
	if handshakeType == "ecdsa" {
		signatureAlgs = []utls.SignatureScheme{
			utls.ECDSAWithP256AndSHA256,
			utls.ECDSAWithP384AndSHA384,
			utls.ECDSAWithP521AndSHA512,
		}
	} else if handshakeType == "rsa" {
		signatureAlgs = []utls.SignatureScheme{
			utls.PKCS1WithSHA256,
			utls.PKCS1WithSHA384,
			utls.PKCS1WithSHA512,
			utls.PSSWithSHA256,
			utls.PSSWithSHA384,
			utls.PSSWithSHA512,
		}
	}

	tlsConfig := &utls.Config{
		ServerName:         hostname,
		InsecureSkipVerify: true, // Allow all certs, including expired/invalid
	}
	uconn := utls.UClient(conn, tlsConfig, utls.HelloCustom)
	spec := &utls.ClientHelloSpec{
		CipherSuites: suites,
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{ServerName: hostname},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}}, // uncompressed
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: signatureAlgs},
			&utls.ALPNExtension{AlpnProtocols: []string{"h2", "http/1.1", "http/1.0", "h3", "spdy/3.1", "acme-tls/1"}},
		},
	}
	if err := uconn.ApplyPreset(spec); err != nil {
		return nil, err
	}
	if err := uconn.Handshake(); err != nil {
		return nil, err
	}

	certs := []string{}
	state := uconn.ConnectionState()
	for _, cert := range state.PeerCertificates {
		certs = append(certs, base64.StdEncoding.EncodeToString(cert.Raw))
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no certs found")
	}
	return &ScanResult{
		IP:            ip,
		Port:          port,
		Hostname:      hostname,
		HandshakeType: handshakeType,
		Certificates:  certs,
		Timestamp:     time.Now().Unix(),
	}, nil
}

// ProtocolHandler defines a function type for protocol-specific scan logic.
// Returns true if handled, false to fall back to default TLS scan.
type ProtocolHandler func(ip, hostname string, port int) (handled bool)

// protocolHandlers maps protocol names to their handler functions.
// Handlers for protocols like smtp, imap, pop3, ldap, and custom can be extended modularly.
// For http1/h2/h3, the defaultTLSHandler is used to perform ECDSA and RSA handshakes.
var protocolHandlers = map[string]ProtocolHandler{
	"smtp": smtpProtocolHandler,
	"imap": func(ip, hostname string, port int) bool {
		logutil.DebugLog("IMAP protocol handler not implemented for %s:%d", ip, port)
		return false
	},
	"pop3": func(ip, hostname string, port int) bool {
		logutil.DebugLog("POP3 protocol handler not implemented for %s:%d", ip, port)
		return false
	},
	"ldap": func(ip, hostname string, port int) bool {
		logutil.DebugLog("LDAP protocol handler not implemented for %s:%d", ip, port)
		return false
	},
	"custom": func(ip, hostname string, port int) bool {
		logutil.DebugLog("Custom protocol handler not implemented for %s:%d", ip, port)
		return false
	},
	// Default handler for HTTP and other protocols: ECDSA & RSA handshake
	"http1": func(ip, hostname string, port int) bool {
		return defaultTLSHandler(ip, hostname, port, "http1")
	},
	"h2": func(ip, hostname string, port int) bool {
		return defaultTLSHandler(ip, hostname, port, "h2")
	},
	"h3": func(ip, hostname string, port int) bool {
		return defaultTLSHandler(ip, hostname, port, "h3")
	},
}

// defaultTLSHandler performs ECDSA and RSA handshakes for a given protocol and sends results to the webhook.
// Used as the default handler for web protocols (http1, h2, h3).
// Parameters:
//
//	ip:      Target IP address
//	hostname: Hostname/SNI
//	port:    Target port
//	proto:   Protocol string
//
// Returns: true (always handles)
func defaultTLSHandler(ip, hostname string, port int, proto string) bool {
	webhookURL := shared.Config.WebhookURL
	dialTimeout := time.Duration(shared.Config.DialTimeoutMs)
	dialTimeout = dialTimeout * time.Millisecond

	var results []ScanResult

	ecdsaSuites := []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA,
	}
	if result, err := tlsHandshakeAndCollectWithTimeout(ip, hostname, port, ecdsaSuites, "ecdsa", proto, dialTimeout); err == nil {
		results = append(results, *result)
	} else {
		logutil.DebugLog("ECDSA handshake failed: %v", err)
	}

	rsaSuites := []uint16{
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA,
		tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA256,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
		tls.TLS_RSA_WITH_RC4_128_SHA,
	}
	if result, err := tlsHandshakeAndCollectWithTimeout(ip, hostname, port, rsaSuites, "rsa", proto, dialTimeout); err == nil {
		results = append(results, *result)
	} else {
		logutil.DebugLog("RSA handshake failed: %v", err)
	}

	// Filter certificates based on exclude_certs rules
	excludeCerts := shared.Config.ExcludeCerts
	for i := range results {
		results[i].Certificates = filterCerts(decodeBase64Certs(results[i].Certificates), excludeCerts)
	}

	if len(results) > 0 {
		sendToWebhook(results, webhookURL)
	}
	return true
}

// ScanAndSendWithProtocol scans each port using the specified protocol, supporting concurrency and protocol handlers.
// Uses a semaphore to limit concurrency and supports protocol-specific logic via protocolHandlers.
// Parameters:
//
//	ip:       Target IP address
//	hostname: Hostname/SNI
//	ports:    List of ports to scan
//	protocol: Protocol string (e.g., "http1", "smtp")
func ScanAndSendWithProtocol(ip, hostname string, ports []int, protocol string) {
	var results []ScanResult
	webhookURL := shared.Config.WebhookURL

	webPorts := map[int]bool{443: true, 8443: true, 4433: true, 5001: true, 10443: true}
	smtpPorts := map[int]bool{25: true, 465: true, 587: true}

	concurrency := shared.Config.ConcurrencyLimit
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	// Use configured dial timeout
	dialTimeout := time.Duration(shared.Config.DialTimeoutMs)
	dialTimeout = dialTimeout * time.Millisecond

	for _, port := range ports {
		wg.Add(1)
		sem <- struct{}{}
		go func(port int) {
			defer wg.Done()
			defer func() { <-sem }()
			proto := protocol
			if proto == "" && webPorts[port] {
				proto = "http1"
			} else if smtpPorts[port] {
				proto = "smtp"
			}

			// check if proto is allowed (check if in AllowedProtocols)
			if proto != "" && !shared.Contains(AllowedProtocols, proto) {
				logutil.DebugLog("Protocol %s not allowed for %s:%d", proto, ip, port)
				return
			}

			// Protocol handler abstraction
			if handler, ok := protocolHandlers[proto]; ok {
				logutil.DebugLog("Scanning %s -> %s:%d (protocol: %s)", ip, hostname, port, proto)
				if handled := handler(ip, hostname, port); handled {
					return
				}
			} else {
				logutil.ErrorLog("No handler for protocol %s, falling back to default TLS scan", proto)
			}
		}(port)
	}
	wg.Wait()

	if len(results) > 0 {
		sendToWebhook(results, webhookURL)
	}
}

// ScanAndSend is a compatibility helper for legacy code paths.
// It loops over the given ports, determines the protocol for each port,
// and calls ScanAndSendWithProtocol for each port/protocol combination.
func ScanAndSend(ip, host string, ports []int) {
	webPorts := map[int]string{443: "http1", 8443: "http1", 4433: "http1", 10443: "http1", 5001: "http1"}
	smtpPorts := map[int]string{25: "smtp", 465: "smtp", 587: "smtp"}
	for _, port := range ports {
		proto := "http1" // default protocol is http1
		if p, ok := webPorts[port]; ok {
			proto = p
		} else if p, ok := smtpPorts[port]; ok {
			proto = p
		}
		ScanAndSendWithProtocol(ip, host, []int{port}, proto)
	}
}

// Add helper:
func decodeBase64Certs(b64certs []string) [][]byte {
	var out [][]byte
	for _, b64 := range b64certs {
		if der, err := base64.StdEncoding.DecodeString(b64); err == nil {
			out = append(out, der)
		}
	}
	return out
}
