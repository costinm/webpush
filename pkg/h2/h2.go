package h2

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/costinm/wpgate/pkg/auth"
	"github.com/costinm/wpgate/pkg/mesh"
	"golang.org/x/net/http2"
)

// H2 provides network communication over HTTP/2, QUIC, SSH
// It also handles the basic config loading - in particular certificates.
//
type H2 struct {
	quicClientsMux sync.RWMutex
	quicClients    map[string]*http.Client

	// HttpsClient with mesh certificates, H2.
	// Call Client() to get it - or the Quic one
	httpsClient *http.Client

	VIP6 net.IP

	Vpn string

	// Local mux is exposed on 127.0.0.1:5227
	LocalMux *http.ServeMux

	// MTLS mux.
	// In DMesh it exposes register, tcp, admin
	MTLSMux *http.ServeMux

	// Client tls config, shared
	tlsConfig *tls.Config

	//GrpcServer http.Handler

	Certs *auth.Auth
}

var (
	// Set to the address of the AP master
	AndroidAPMaster string
	AndroidAPIface  *net.Interface
	AndroidAPLL     net.IP
)

// Deprecated, test only
func NewH2(confdir string) (*H2, error) {
	name, _ := os.Hostname()
	certs := auth.NewAuth(nil, name, "m.webinf.info")
	return NewTransport(certs)
}

func NewTransport(authz *auth.Auth) (*H2, error) {
	h2 := &H2{
		MTLSMux:     &http.ServeMux{},
		LocalMux:    &http.ServeMux{},
		quicClients: map[string]*http.Client{},
	}

	h2.Certs = authz

	h2.VIP6 = auth.Pub2VIP(h2.Certs.Pub)

	ctls := h2.Certs.GenerateTLSConfigClient()
	ctls.VerifyPeerCertificate = verify("")
	h2.tlsConfig = ctls

	t := &http.Transport{
		// This is enough to disable h2 automatically.
		TLSClientConfig: ctls,
	}

	http2.ConfigureTransport(t)
	rtt := http.RoundTripper(t)

	if mesh.MetricsClientTransportWrapper != nil {
		rtt = mesh.MetricsClientTransportWrapper(rtt)
	}

	h2.httpsClient = &http.Client{
		Timeout: 15 * time.Minute,
		//Timeout:   5 * time.Second,
		Transport: rtt,
	}

	return h2, nil
}

func CleanQuic(httpClient *http.Client) {
	//hrt, ok := httpClient.Transport.(*h2quic.RoundTripper)
	hrt, ok := httpClient.Transport.(io.Closer)
	if ok {
		hrt.Close()
	}
}

// Start QUIC and HTTPS servers on port, using handler.
func (h2 *H2) InitMTLSServer(port int, handler http.Handler) error {
	if mesh.MetricsHandlerWrapper != nil {
		handler = mesh.MetricsHandlerWrapper(handler)
	}
	err := h2.InitH2Server(":"+strconv.Itoa(port), handler, true)
	if err != nil {
		return err
	}
	if UseQuic {
		err = h2.InitQuicServer(port, handler)
	}
	return err
}

func (h2 *H2) InitH2Server(port string, handler http.Handler, mtls bool) error {
	tcpAddr, err := net.ResolveTCPAddr("tcp", port)
	if err != nil {
		log.Println("Failed to resolve ", port)
		return err
	}
	tcpConn, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		log.Println("Failed to listen https ", port)
		return err
	}
	return h2.initH2ServerListener(tcpConn, handler, mtls)
}

func (h2 *H2) initH2ServerListener(tcpConn *net.TCPListener, handler http.Handler, mtls bool) error {

	tlsServerConfig := h2.Certs.GenerateTLSConfigServer()
	if mtls {
		tlsServerConfig.ClientAuth = tls.RequireAnyClientCert // only option supported by mint?
	}
	hw := h2.handlerWrapper(handler)
	hw.mtls = mtls
	// Self-signed cert
	s := &http.Server{
		TLSConfig: tlsServerConfig,
		Handler:   hw,
	}

	// Regular TLS
	tlsConn := tls.NewListener(tcpConn, tlsServerConfig)
	go s.Serve(tlsConn)

	return nil
}

// Verify a server
func verify(pub string) func(der [][]byte, verifiedChains [][]*x509.Certificate) error {
	return func(der [][]byte, verifiedChains [][]*x509.Certificate) error {
		var err error
		x509Cert := make([]*x509.Certificate, len(der))
		for i, b := range der {
			// err already checked
			x509Cert[i], _ = x509.ParseCertificate(b)
		}

		// verify the leaf is not expired
		leaf := x509Cert[0]
		now := time.Now()
		if now.Before(leaf.NotBefore) {
			return errors.New("certificate is not valid yet")
		}
		if now.After(leaf.NotAfter) {
			return errors.New("expired certificate")
		}

		// TODO: match the pub key against the trust DB
		// certs are self-signed, and domain name is not trusted - just the pub key

		// Use the equivalent of SSH known-hosts as database.

		return err
	}
}

func traceMap(r *http.Request) string {
	p := r.URL.Path
	// TODO: move to main
	if strings.HasPrefix(p, "/tcp/") {
		return "/tcp"
	}
	if strings.HasPrefix(p, "/dm/") {
		return "/dm"
	}

	return r.URL.Path
}

// handler wrapper wraps a Handler, adding MTLS checking, recovery, metrics.
type handlerWrapper struct {
	handler http.Handler
	h2      *H2
	mtls    bool
}

func (h2 *H2) handlerWrapper(h http.Handler) *handlerWrapper { // http.Handler {
	return &handlerWrapper{handler: h, h2: h2, mtls: true}
}

func (hw *handlerWrapper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	// TODO: authenticate first, either localhost (for proxy) or JWT/clientcert
	// TODO: split localhost to different method ?
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("Recovered in f", r)

			debug.PrintStack()

			// find out exactly what the error was and set err
			var err error

			switch x := r.(type) {
			case string:
				err = errors.New(x)
			case error:
				err = x
			default:
				err = errors.New("Unknown panic")
			}
			if err != nil {
				fmt.Println("ERRROR: ", err)
			}
		}
	}()

	var vip net.IP
	var san []string
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		if hw.mtls {
			log.Println("403 NO_MTLS", r.RemoteAddr, r.URL)
			w.WriteHeader(403)
			return
		}
	}
	pk1 := r.TLS.PeerCertificates[0].PublicKey
	pk1b := auth.KeyBytes(pk1)
	vip = auth.Pub2VIP(pk1b)
	var role string

	// ssh-style, known pub leaf
	if role = hw.h2.Certs.Authorized[string(pk1b)]; role == "" {
		role = "guest"
	}

	// TODO: Istio-style, signed by a trusted CA. This is also for SSH-with-cert

	san, _ = GetSAN(r.TLS.PeerCertificates[0])

	// TODO: check role

	ctx := context.WithValue(r.Context(), H2Info, &H2Context{
		SAN:  san,
		Role: role,
		T0:   t0,
	})
	//if hw.h2.GrpcServer != nil && r.ProtoMajor == 2 && strings.HasPrefix(
	//	r.Header.Get("Content-Type"), "application/grpc") {
	//	hw.h2.GrpcServer.ServeHTTP(w, r)
	//}
	hw.handler.ServeHTTP(w, r.WithContext(ctx))

	// TODO: add it to an event buffer
	if accessLogs && !strings.Contains(r.URL.Path, "/dns") {
		log.Println("HTTP", san, vip, r.RemoteAddr, r.URL, time.Since(t0))
	}
}

// Common RBAC/Policy
//
// Input: context - VIP6 src/dest, ports, etc.
// For HTTP: path
//
// Map to 'group'
//
// Use Authorized_keys or groups to match

type H2Key int

var (
	H2Info     = H2Key(1)
	accessLogs = false
)

type H2Context struct {
	// Auth role
	Role string

	// SAN list from the certificate, or equivalent auth method.
	SAN []string

	// Request start time
	T0 time.Time
}

func GetPeerCertBytes(r *http.Request) []byte {
	if r.TLS != nil {
		if len(r.TLS.PeerCertificates) > 0 {
			pke, ok := r.TLS.PeerCertificates[0].PublicKey.(*ecdsa.PublicKey)
			if ok {
				return elliptic.Marshal(auth.Curve256, pke.X, pke.Y)
			}
			rsap, ok := r.TLS.PeerCertificates[0].PublicKey.(*rsa.PublicKey)
			if ok {
				return x509.MarshalPKCS1PublicKey(rsap)
			}
		}
	}
	return nil
}

func GetResponseCertBytes(r *http.Response) []byte {
	if r.TLS != nil {
		if len(r.TLS.PeerCertificates) > 0 {
			pke, ok := r.TLS.PeerCertificates[0].PublicKey.(*ecdsa.PublicKey)
			if ok {
				return elliptic.Marshal(auth.Curve256, pke.X, pke.Y)
			}
			rsap, ok := r.TLS.PeerCertificates[0].PublicKey.(*rsa.PublicKey)
			if ok {
				return x509.MarshalPKCS1PublicKey(rsap)
			}
		}
	}
	return nil
}

var (
	oidExtensionSubjectAltName = []int{2, 5, 29, 17}
)

const (
	nameTypeEmail = 1
	nameTypeDNS   = 2
	nameTypeURI   = 6
	nameTypeIP    = 7
)

func getSANExtension(c *x509.Certificate) []byte {
	for _, e := range c.Extensions {
		if e.Id.Equal(oidExtensionSubjectAltName) {
			return e.Value
		}
	}
	return nil
}

func GetSAN(c *x509.Certificate) ([]string, error) {
	extension := getSANExtension(c)
	dns := []string{}
	// RFC 5280, 4.2.1.6

	// SubjectAltName ::= GeneralNames
	//
	// GeneralNames ::= SEQUENCE SIZE (1..MAX) OF GeneralName
	//
	// GeneralName ::= CHOICE {
	//      otherName                       [0]     OtherName,
	//      rfc822Name                      [1]     IA5String,
	//      dNSName                         [2]     IA5String,
	//      x400Address                     [3]     ORAddress,
	//      directoryName                   [4]     Name,
	//      ediPartyName                    [5]     EDIPartyName,
	//      uniformResourceIdentifier       [6]     IA5String,
	//      iPAddress                       [7]     OCTET STRING,
	//      registeredID                    [8]     OBJECT IDENTIFIER }
	var seq asn1.RawValue
	rest, err := asn1.Unmarshal(extension, &seq)
	if err != nil {
		return dns, err
	} else if len(rest) != 0 {
		return dns, errors.New("x509: trailing data after X.509 extension")
	}
	if !seq.IsCompound || seq.Tag != 16 || seq.Class != 0 {
		return dns, asn1.StructuralError{Msg: "bad SAN sequence"}
	}

	rest = seq.Bytes
	for len(rest) > 0 {
		var v asn1.RawValue
		rest, err = asn1.Unmarshal(rest, &v)
		if err != nil {
			return dns, err
		}

		if v.Tag == nameTypeDNS {
			dns = append(dns, string(v.Bytes))
		}
	}

	return dns, nil
}

// NewSocksHttpClient returns a new client using SOCKS5 server.
func NewSocksHttpClient(socksAddr string) *http.Client {
	if socksAddr == "" {
		socksAddr = "127.0.0.1:15004"
	}
	//os.Setenv("HTTP_PROXY", "socks5://"+socks5Addr)
	// Localhost is not accepted by environment.
	//hc := &http.Client{Transport: &http.Transport{Gateway: http.ProxyFromEnvironment}}

	// Configure a hcSocks http client using localhost SOCKS
	socksProxy, _ := url.Parse("socks5://" + socksAddr)
	return &http.Client{
		Timeout: 15 * time.Minute,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(socksProxy),
			//TLSClientConfig: &tls.Config{
			//	InsecureSkipVerify: true,
			//},
		},
	}
}

func InitServer(port string) (err error) {
	lis, err := net.Listen("tcp", port)
	if err != nil {
		return
	}
	for {
		conn, err := lis.Accept()
		if err != nil {
			return err
		}
		go handleCon(conn)
	}
}

// Special handler for receipts and poll, which use push promises
func handleCon(con net.Conn) {
	defer con.Close()
	// writer: bufio.NewWriterSize(conn, http2IOBufSize),
	f := http2.NewFramer(con, con)
	settings := []http2.Setting{}

	if err := f.WriteSettings(settings...); err != nil {
		return
	}

	frame, err := f.ReadFrame()
	if err != nil {
		log.Println(" failed to read frame", err)
		return
	}
	sf, ok := frame.(*http2.SettingsFrame)
	if !ok {
		log.Printf("wrong frame %T from client", frame)
		return
	}
	log.Println(sf)
	//hDec := hpack.NewDecoder()

	for {
		select {}

	}
}