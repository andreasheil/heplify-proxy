package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/big"
	"net"
	"os"
	"sync"
	"time"
)

var (
	// sign certificates for 7 years in the past -> 7 years in the future
	notBefore = time.Now().Add(-7 * 365 * 24 * time.Hour)
	notAfter  = time.Now().Add(7 * 365 * 24 * time.Hour)
)

func defaultServerName() string {
	dsn, err := os.Hostname()
	if err != nil {
		dsn = "heplify-proxy"
	}
	return dsn
}

func pemBlockForKey(priv interface{}) *pem.Block {
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		return &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}
	case *ecdsa.PrivateKey:
		b, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Unable to marshal ECDSA private key: %v", err)
			os.Exit(2)
		}
		return &pem.Block{Type: "EC PRIVATE KEY", Bytes: b}
	default:
		return nil
	}
}

// CertificateAuthority implements crypto.Signer and tls.Config/GetCertificate
type CertificateAuthority struct {
	cert  tls.Certificate
	store map[string]*tls.Certificate
}

// NewCertificateAuthority returns a certificate authority.
// First, we attempt to load a CA from the ca_cert.pem and ca_key.pem files.
// If this does not succeed, we generate a new CA and save it to disk.
func NewCertificateAuthority() *CertificateAuthority {
	ca, err := CertificateAuthorityFromFile()
	if err != nil {
		ca, err = CertificateAuthorityFromScratch()
		if err != nil {
			log.Fatal(err)
		}
	}
	return ca
}

// CertificateAuthorityFromScratch generates a new certificate authority
// and saves the private key and certificate pair to disk.
func CertificateAuthorityFromScratch() (*CertificateAuthority, error) {
	// generate a crypto.Signer
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	// generate a random serial number for the certificate
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, err
	}

	// create the CSR for our Certificate Authority
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"NetCologne"},
			CommonName:   "NetCologne GmbH",
		},

		NotBefore: notBefore,
		NotAfter:  notAfter,

		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA: true,
	}

	// self sign the generated certificate
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}

	// write the certificate to disk
	certOut, err := os.Create("ca_cert.pem")
	if err != nil {
		return nil, err
	}
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	certOut.Close()
	log.Printf("wrote certificate authority to ca_cert.pem")

	// write the private key to disk
	keyOut, err := os.OpenFile("ca_key.pem", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}
	pem.Encode(keyOut, pemBlockForKey(priv))
	keyOut.Close()
	log.Printf("wrote private key to ca_key.pem")

	// return the certificate authority by reading from disk
	return CertificateAuthorityFromFile()
}

// CertificateAuthorityFromFile loads a certificate authority from disk.
func CertificateAuthorityFromFile() (*CertificateAuthority, error) {
	cert, err := tls.LoadX509KeyPair("ca_cert.pem", "ca_key.pem")
	if err != nil {
		return nil, err
	}
	pem.Encode(ioutil.Discard, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})

	return &CertificateAuthority{
		cert:  cert,
		store: make(map[string]*tls.Certificate),
	}, nil
}

// GetCertificate returns a Certificate based on the given
// ClientHelloInfo.ServerName. As described by crypto.tls, it will
// only be called if the client supplies SNI information.
func (ca *CertificateAuthority) GetCertificate(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
	// fallback to default if SNI is empty
	if h.ServerName == "" {
		h.ServerName = defaultServerName()
	}
	log.Printf("%s -> %s", h.Conn.RemoteAddr(), h.ServerName)

	// fetch previously signed certificate from storage if it exists
	if cert, ok := ca.store[h.ServerName]; ok {
		return cert, nil
	}

	// generate a crypto.Signer
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	// generate a random serial number for the certificate
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, err
	}

	// create the CSR for our Certificate Authority
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"NetCologne"},
			CommonName:   h.ServerName,
		},

		NotBefore: notBefore,
		NotAfter:  notAfter,

		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,

		DNSNames: []string{h.ServerName},
	}

	// sign the generated certificate
	parent, err := x509.ParseCertificate(ca.cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, parent, &priv.PublicKey, ca.cert.PrivateKey)
	if err != nil {
		return nil, err
	}

	// save the certificate chain to storage and return
	cert := &tls.Certificate{
		Certificate: [][]byte{derBytes, ca.cert.Certificate[0]},
		PrivateKey:  priv,
	}
	ca.store[h.ServerName] = cert
	return cert, nil
}

func serveTLS() {
	ca := NewCertificateAuthority()

	l, err := tls.Listen("tcp", *lAddrTCP, &tls.Config{
		GetCertificate: ca.GetCertificate,
	})
	panicIfErr(err)

	defer l.Close()
	for {
		// Wait for a connection.
		conn, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}

		// Handle the connection in a new goroutine.
		go func(c net.Conn) {
			proxy, err := net.Dial("udp", *rAddr)
			if err != nil {
				log.Print(err)
			}

			// Close the connection once.
			var once sync.Once
			onceBody := func() {
				c.Close()
				proxy.Close()
			}

			// Read from conn, send to proxy.
			go func(c net.Conn) {
				io.Copy(proxy, c)
				once.Do(onceBody)
			}(c)

			// Read from proxy, send to conn.
			go func(c net.Conn) {
				io.Copy(c, proxy)
				once.Do(onceBody)
			}(c)
		}(conn)
	}
}
