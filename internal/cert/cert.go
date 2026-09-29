package cert

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"path/filepath"
	"os"
	"time"
	"io"
	"net/http"
)

var (
	CertPath = "/etc/peakroute/certs/tls.crt"
	KeyPath  = "/etc/peakroute/certs/tls.key"
)

func EnableDevPaths(dir string) {
	os.MkdirAll(dir, 0700)
	CertPath = dir + "/tls.crt"
	KeyPath = dir + "/tls.key"
}

// EnsureTLSCert verifies the existence and validity of the TLS cert.
// Automatically regenerates self-signed certs if expired or near expiration.
func EnsureTLSCert() error {
	// If files don't exist yet, generate fresh self-signed cert
	if _, err := os.Stat(CertPath); os.IsNotExist(err) {
		return GenerateSelfSignedCert()
	}

	// Read existing cert to check validity and issuer
	certBytes, err := os.ReadFile(CertPath)
	if err != nil {
		return fmt.Errorf("failed to read cert: %w", err)
	}

	block, _ := pem.Decode(certBytes)
	if block == nil {
		fmt.Println("[!] Corrupted cert file detected. Regenerating...")
		return GenerateSelfSignedCert()
	}

	parsedCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		fmt.Println("[!] Failed to parse x509 cert. Regenerating...")
		return GenerateSelfSignedCert()
	}

	// DO NOT auto-renew custom/uploaded certificates.
	// We check if the Issuer matches our auto-generated subject line safely.
	isSelfSigned := parsedCert.Issuer.CommonName == "PeakRoute OS Local CA" ||
	(len(parsedCert.Issuer.Organization) > 0 && parsedCert.Issuer.Organization[0] == "PeakRoute OS")

	if !isSelfSigned {
		fmt.Println("[+] Custom TLS certificate detected. Skipping auto-renewal.")
		return nil
	}

	// Auto-renew if within 30 days of expiration or already expired
	timeUntilExpiry := time.Until(parsedCert.NotAfter)
	if timeUntilExpiry < 30*24*time.Hour {
		fmt.Printf("[!] Self-signed TLS cert expiring in %v days. Auto-renewing...\n", int(timeUntilExpiry.Hours()/24))
		return GenerateSelfSignedCert()
	}

	fmt.Printf("[+] TLS cert valid (expires in %d days)\n", int(timeUntilExpiry.Hours()/24))

	return nil
}

// GenerateSelfSignedCert issues a 365-day self-signed certificate.
func GenerateSelfSignedCert() error {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("failed to generate RSA key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return fmt.Errorf("failed to generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"PeakRoute OS"},
			CommonName:   "PeakRoute OS Local CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(1, 0, 0), // Valid for 1 year (365 days)
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("failed to create certificate: %w", err)
	}

	certDir := filepath.Dir(CertPath)
	if err := os.MkdirAll(certDir, 0755); err != nil {
		return fmt.Errorf("failed to create certificate directory %s: %w", certDir, err)
	}

	// Save Cert
	certOut, err := os.Create(CertPath)
	if err != nil {
		return fmt.Errorf("failed to open cert file for writing: %w", err)
	}
	defer certOut.Close()
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	// Save Key
	keyOut, err := os.OpenFile(KeyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to open key file for writing: %w", err)
	}
	defer keyOut.Close()

	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}
	pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})

	fmt.Println("[+] Successfully generated/renewed 1-year self-signed TLS cert.")
	return nil
}

// HandleUploadCert processes user-uploaded custom TLS certificate and key files.
func HandleUploadCert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit upload size to 1MB to prevent abuse
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	// Parse multipart form (expecting 'cert' and 'key' file inputs)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, "File upload too large or invalid multipart form", http.StatusBadRequest)
		return
	}

	// Read cert file
	certFile, _, err := r.FormFile("cert")
	if err != nil {
		http.Error(w, "Missing 'cert' file field", http.StatusBadRequest)
		return
	}
	defer certFile.Close()

	// Read key file
	keyFile, _, err := r.FormFile("key")
	if err != nil {
		http.Error(w, "Missing 'key' file field", http.StatusBadRequest)
		return
	}
	defer keyFile.Close()

	certBytes, err := io.ReadAll(certFile)
	if err != nil {
		http.Error(w, "Failed to read certificate file", http.StatusInternalServerError)
		return
	}

	keyBytes, err := io.ReadAll(keyFile)
	if err != nil {
		http.Error(w, "Failed to read private key file", http.StatusInternalServerError)
		return
	}

	// Save custom cert and key directly to defined paths
	if err := os.WriteFile(CertPath, certBytes, 0644); err != nil {
		http.Error(w, "Failed to write certificate to disk", http.StatusInternalServerError)
		return
	}

	if err := os.WriteFile(KeyPath, keyBytes, 0600); err != nil {
		http.Error(w, "Failed to write private key to disk", http.StatusInternalServerError)
		return
	}

	fmt.Println("[+] Custom TLS certificate successfully uploaded and installed.")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"success","message":"Certificate uploaded. Restart daemon or service to apply changes."}`))
}
