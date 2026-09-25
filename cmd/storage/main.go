package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"storage/internal/s3api"
	"storage/internal/store"
)

func main() {
	addr := flag.String("addr", listenAddr(), "dirección de escucha")
	data := flag.String("data", env("STORAGE_DATA", "data"), "directorio de datos")
	region := flag.String("region", env("STORAGE_REGION", "us-east-1"), "región que ven los clientes")
	access := flag.String("access-key", os.Getenv("STORAGE_ACCESS_KEY"), "access key (si se omite, se carga o se crea en el directorio de datos)")
	secret := flag.String("secret-key", os.Getenv("STORAGE_SECRET_KEY"), "secret key")
	publicURL := flag.String("public-url", env("STORAGE_PUBLIC_URL", "http://localhost:9000"), "URL que usan los clientes")
	tlsCert := flag.String("tls-cert", os.Getenv("STORAGE_TLS_CERT"), "certificado TLS")
	tlsKey := flag.String("tls-key", os.Getenv("STORAGE_TLS_KEY"), "llave TLS")
	maxObject := flag.Int64("max-object-bytes", 8<<30, "tamaño máximo de un objeto")
	flag.Parse()

	accessKey, secretKey, generated, err := loadKeys(*data, *access, *secret)
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(*data)
	if err != nil {
		log.Fatal(err)
	}
	srv := &s3api.Server{
		Store:     st,
		AccessKey: accessKey,
		SecretKey: secretKey,
		Region:    *region,
		PublicURL: strings.TrimRight(*publicURL, "/"),
		Bases:     baseHosts(*addr, *publicURL),
		MaxObject: *maxObject,
	}
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	errCh := make(chan error, 1)
	go func() {
		var err error
		if *tlsCert != "" || *tlsKey != "" {
			err = httpSrv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = httpSrv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	fmt.Printf("storage listo\n")
	fmt.Printf("  endpoint    %s\n", strings.TrimRight(*publicURL, "/"))
	fmt.Printf("  escuchando  %s\n", *addr)
	fmt.Printf("  region      %s\n", *region)
	fmt.Printf("  access key  %s\n", accessKey)
	if generated {
		fmt.Printf("  secret key  %s\n", secretKey)
		fmt.Printf("  credenciales guardadas en %s\n", filepath.Join(*data, "credentials.json"))
	} else {
		fmt.Printf("  secret key  (no se imprime; ya estaba configurado)\n")
	}
	fmt.Printf("  datos       %s\n", *data)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shut)
	case err := <-errCh:
		log.Fatal(err)
	}
}

func loadKeys(data, access, secret string) (string, string, bool, error) {
	if access != "" || secret != "" {
		if access == "" || secret == "" {
			return "", "", false, fmt.Errorf("access key y secret key van juntos")
		}
		return access, secret, false, nil
	}
	path := filepath.Join(data, "credentials.json")
	b, err := os.ReadFile(path)
	if err == nil {
		var cred struct {
			AccessKey string `json:"accessKey"`
			SecretKey string `json:"secretKey"`
		}
		if err := json.Unmarshal(b, &cred); err != nil {
			return "", "", false, err
		}
		if cred.AccessKey == "" || cred.SecretKey == "" {
			return "", "", false, fmt.Errorf("credentials.json incompleto")
		}
		return cred.AccessKey, cred.SecretKey, false, nil
	}
	if !os.IsNotExist(err) {
		return "", "", false, err
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		return "", "", false, err
	}
	access, secret, err = newKeys()
	if err != nil {
		return "", "", false, err
	}
	raw, err := json.MarshalIndent(map[string]string{"accessKey": access, "secretKey": secret}, "", "  ")
	if err != nil {
		return "", "", false, err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return "", "", false, err
	}
	return access, secret, true, nil
}

func newKeys() (string, string, error) {
	access, err := randAlphabet(18)
	if err != nil {
		return "", "", err
	}
	buf := make([]byte, 30)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	return "ST" + access, base64.RawURLEncoding.EncodeToString(buf), nil
}

func randAlphabet(n int) (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf), nil
}

func baseHosts(addr, publicURL string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(h string) {
		h = strings.TrimSpace(strings.ToLower(h))
		if h == "" {
			return
		}
		if host, _, err := net.SplitHostPort(h); err == nil {
			h = host
		}
		if _, ok := seen[h]; ok {
			return
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	add("localhost")
	add("127.0.0.1")
	add(addr)
	if u, err := url.Parse(publicURL); err == nil {
		add(u.Host)
	}
	return out
}

// listenAddr uses STORAGE_ADDR when set. Otherwise PORT (Easypanel and other
// panels) binds every interface. A local run without either stays on localhost.
func listenAddr() string {
	if v := os.Getenv("STORAGE_ADDR"); v != "" {
		return v
	}
	if p := os.Getenv("PORT"); p != "" {
		if strings.Contains(p, ":") {
			return p
		}
		return "0.0.0.0:" + p
	}
	return "127.0.0.1:9000"
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
