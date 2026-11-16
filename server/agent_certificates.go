package main

import (
	"archive/zip"
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"monitor/internal/transport"
)

const maxAgentCertificateDays = 365

type agentIssuer struct {
	certificate *x509.Certificate
	signer      crypto.Signer
	clientTrust x509.VerifyOptions
	serverLeaf  *x509.Certificate
	serverTrust x509.VerifyOptions
	serverCA    []byte
}

func certificateIntermediates(pair tls.Certificate) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		pool.AddCert(cert)
	}
	return pool, nil
}

func loadAgentIssuer(certPath, keyPath, serverCAPath string, cfg *tls.Config) (*agentIssuer, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load Agent signing CA: %w", err)
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok || !pair.Leaf.BasicConstraintsValid || !pair.Leaf.IsCA || pair.Leaf.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, errors.New("Agent signing certificate must be a CA authorized to sign certificates")
	}
	issuers, err := certificateIntermediates(pair)
	if err != nil {
		return nil, fmt.Errorf("load Agent signing CA chain: %w", err)
	}
	clientTrust := x509.VerifyOptions{Roots: cfg.ClientCAs, Intermediates: issuers, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if _, err := pair.Leaf.Verify(clientTrust); err != nil {
		return nil, fmt.Errorf("Agent signing CA must be trusted by tls-ca for client authentication: %w", err)
	}

	raw, err := os.ReadFile(serverCAPath)
	if err != nil {
		return nil, fmt.Errorf("load public CA bundle for Agents: %w", err)
	}
	roots := x509.NewCertPool()
	// Re-encode parsed certificates only. Never send the original file to the browser.
	var publicCA bytes.Buffer
	for len(raw) > 0 {
		block, rest := pem.Decode(raw)
		if block == nil {
			break
		}
		raw = rest
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse public CA bundle for Agents: %w", err)
		}
		roots.AddCert(cert)
		if err := pem.Encode(&publicCA, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}); err != nil {
			return nil, err
		}
	}
	if publicCA.Len() == 0 {
		return nil, errors.New("public CA bundle for Agents contains no certificates")
	}
	server := cfg.Certificates[0]
	serverChain, err := certificateIntermediates(server)
	if err != nil {
		return nil, fmt.Errorf("load gRPC Server certificate chain: %w", err)
	}
	serverTrust := x509.VerifyOptions{Roots: roots, Intermediates: serverChain}
	if _, err := server.Leaf.Verify(serverTrust); err != nil {
		return nil, fmt.Errorf("agent-server-ca must verify the gRPC Server certificate: %w", err)
	}
	return &agentIssuer{pair.Leaf, signer, clientTrust, server.Leaf, serverTrust, publicCA.Bytes()}, nil
}

type agentCertificateRequest struct {
	HostID    string `json:"host_id"`
	Server    string `json:"server"`
	Platform  string `json:"platform"`
	ValidDays int    `json:"valid_days"`
}

func (h *hub) agentCertificateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agent-certificates", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":        h.agentIssuer != nil && r.TLS != nil,
			"token_required": h.authToken != "",
		})
	})
	mux.Handle("POST /api/agent-certificates", http.NewCrossOriginProtection().Handler(http.HandlerFunc(h.issueAgentCertificate)))
}

func (h *hub) issueAgentCertificate(w http.ResponseWriter, r *http.Request) {
	if h.agentIssuer == nil || r.TLS == nil {
		writeError(w, http.StatusForbidden, "网页签发未启用或未通过 HTTPS 访问")
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "application/json required")
		return
	}
	var req agentCertificateRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "签发参数格式无效")
		return
	}
	host, port, err := net.SplitHostPort(req.Server)
	portNum, portErr := strconv.ParseUint(port, 10, 16)
	if err != nil || portErr != nil || portNum == 0 || host == "" || len(host) > 253 {
		writeError(w, http.StatusBadRequest, "请输入 Agent 能访问的 gRPC 地址，格式为主机名或 IP 加端口")
		return
	}
	if !transport.ValidHostID(req.HostID) || (req.Platform != "linux" && req.Platform != "windows") ||
		req.ValidDays < 1 || req.ValidDays > maxAgentCertificateDays {
		writeError(w, http.StatusBadRequest, "请检查主机 ID、操作系统和有效天数（1–365）")
		return
	}
	now := time.Now().UTC().Truncate(time.Second)
	a := h.agentIssuer
	serverTrust := a.serverTrust
	serverTrust.CurrentTime, serverTrust.DNSName = now, host
	if _, err := a.serverLeaf.Verify(serverTrust); err != nil {
		writeError(w, http.StatusBadRequest, "gRPC 地址与当前 Server 证书不匹配，或证书已失效")
		return
	}
	expiry := now.AddDate(0, 0, req.ValidDays)
	bundle, serial, err := a.agentBundle(req, now, expiry)
	if err != nil {
		if errors.Is(err, errAgentValidity) {
			writeError(w, http.StatusBadRequest, "请求的有效期超过签发 CA 的有效期，请缩短天数")
			return
		}
		log.Printf("issue Agent certificate host_id=%q: %v", req.HostID, err)
		writeError(w, http.StatusInternalServerError, "签发失败，请检查 Server 日志")
		return
	}
	user, _, _ := r.BasicAuth()
	log.Printf("issued Agent certificate user=%q host_id=%q serial=%s expires=%s", user, req.HostID, serial.Text(16), expiry.Format(time.RFC3339))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="monitor-agent-%s.zip"`, req.HostID))
	w.Header().Set("Content-Length", strconv.Itoa(len(bundle)))
	_, _ = w.Write(bundle)
}

var errAgentValidity = errors.New("requested validity exceeds issuer chain lifetime")

func (a *agentIssuer) agentBundle(req agentCertificateRequest, now, expiry time.Time) ([]byte, *big.Int, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	serial.Add(serial, big.NewInt(1))
	identity, err := url.Parse(transport.HostURIPrefix + req.HostID)
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: req.HostID}, URIs: []*url.URL{identity},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: expiry,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, &key.PublicKey, a.signer)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	trust := a.clientTrust
	trust.CurrentTime = now
	chains, err := leaf.Verify(trust)
	if err != nil {
		return nil, nil, fmt.Errorf("verify new Agent certificate: %w", err)
	}
	var chain []*x509.Certificate
	for _, candidate := range chains {
		valid := true
		for _, cert := range candidate[1:] {
			valid = valid && !expiry.After(cert.NotAfter)
		}
		if valid {
			chain = candidate
			break
		}
	}
	if len(chain) < 2 {
		return nil, nil, errAgentValidity
	}
	var certPEM []byte
	for _, cert := range chain[:len(chain)-1] {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	dir := "/etc/monitor/"
	if req.Platform == "windows" {
		dir = "C:/ProgramData/Monitor/"
	}
	config, err := json.MarshalIndent(map[string]any{
		"server": req.Server, "host-id": req.HostID, "tls-enabled": true,
		"tls-cert": dir + "agent.crt", "tls-key": dir + "agent.key", "tls-ca": dir + "ca.crt",
	}, "", "  ")
	if err != nil {
		return nil, nil, err
	}

	var out bytes.Buffer
	z := zip.NewWriter(&out)
	for _, file := range []struct {
		name string
		data []byte
	}{
		{"agent.json", append(config, '\n')}, {"agent.crt", certPEM},
		{"agent.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})},
		{"ca.crt", a.serverCA},
	} {
		header := &zip.FileHeader{Name: file.name, Method: zip.Deflate}
		header.SetMode(0600)
		dest, err := z.CreateHeader(header)
		if err != nil {
			return nil, nil, err
		}
		if _, err := dest.Write(file.data); err != nil {
			return nil, nil, err
		}
	}
	if err := z.Close(); err != nil {
		return nil, nil, err
	}
	return out.Bytes(), serial, nil
}
