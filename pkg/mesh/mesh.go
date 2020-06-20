package mesh

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

const (
	TopicConnectUP = "connectUP"
)

// Full mesh database and connection management.
type Mesh struct {
	// TODO: add methods to mutate fields.
	MeshMutex sync.RWMutex

	// Direct nodes by interface address (which is derived from public key - last 8 bytes in
	// initial prototype). This includes only directly connected notes - either Wifi on same segment, or VPNs and
	// connected devices.
	Nodes map[uint64]*DMNode

	// If set, the p2p- interface name of the current active AP
	// The name may change - used to adjust the address/zone of nodes.
	// All nodes have LL addresses including the old zone - might be better to remove any known node,
	// and wait for it to reannounce or respond to our announce. It'll take some time to reconnect as well.
	ActiveP2P string

	// Vpns contains trusted VPN servers, or exit points.

	// Vpn is the currently active VPN server. Will be selected from the list of
	// known VPN servers (in future - for now hardcoded to the test server)
	Vpn string

	// Set to the SSID of the main connection. 'w' param in the net status message.
	ConnectedWifi string
	WifiFreq      string
	WifiLevel     string

	// User agent - hostname or android build id or custom.
	UA string

	// Zero if AP is not running. Set to the time when AP started.
	APStartTime time.Time
	APRunning   bool

	// SSID and password of the AP
	AP     string
	APFreq string
	PSK    string

	// List of public IPs detected. Updated by registry code.
	// On a regular SSH server - may be detected using a shell script or dmcli, or known as the address of the
	// server we connected to.
	PublicIPs []string

	// Address of the VPN gateway ( SSH or H2 ), where the device can be reached.
	GWAddr string

	// UpstreamAP is set if a local announce or registration from an AP has been received. In includes the IP4 and IP6
	// of the AP and info. Will be used by Dial to initiate multiplexed connections ( SSH, future QUIC )
	UpstreamAP *DMNode
}

func NewMesh() Mesh {
	return Mesh{
		Nodes: map[uint64]*DMNode{},
		//WifiInfo: &WifiRegistrationInfo{},
	}
}

func (m *Mesh) IsRoot() bool {
	return os.Getenv("VPNROOT") != ""
}

// Information about a node.
// Sent periodically, signed by the origin - for example as a JWT, or UDP
// proto
type NodeAnnounce struct {
	UA string `json:"UA,omitempty"`

	// Non-link local IPs from all interfaces. Includes public internet addresses
	// and Wifi IP4 address. Used to determine if a node is directly connected.
	IPs []*net.UDPAddr `json:"IPs,omitempty"`

	// Set if the node is an active Android AP.
	SSID string `json:"ssid,omitempty"`

	// True if the node is an active Android AP on the interface sending the message.
	// Will trigger special handling in link-local - if the receiving interface is also
	// an android client.
	AP bool `json:"AP,omitempty"`

	Ack bool `json:"ACK,omitempty"`

	// VIP of the direct parent, if this node is connected.
	// Used to determine the mesh topology.
	Vpn string `json:"Vpn,omitempty"`
}

// Node information, based on registration info or discovery.
// Map of nodes, keyed by interface address is stored in Gateway.Nodes.
type DMNode struct {
	// VIP is the mesh specific IP6 address. The 'network' identifies the master node, the
	// link part is the sha of the public key. This is a byte[16].
	// Last 8 bytes as uint64 are the primary key in the map.
	VIP net.IP `json:"vip,omitempty"`

	// Pub
	PublicKey []byte `json:"pub,omitempty"`

	// Last packet or registration from the peer.
	LastSeen time.Time `json:"t"`

	// Last LL GW address used by the peer.
	// Public IP addresses are stored in Reg.IPs.
	// If set, returned as the first address in GWs, which is used to connect.
	// This is not sent in the registration - but extracted from the request
	// remote address.
	GW *net.UDPAddr `json:"gw,omitempty"`

	Bacokff time.Duration `json:"-"`

	Labels map[string]string `json:"l,omitempty"`

	// Set if this remote node has an active incoming TUN.
	TunSrv TunDialer `json:"-"`

	// Existing tun to the remote node, previously dialed.
	TunClient TunDialer `json:"-"`

	FirstSeen time.Time

	// In seconds since first seen, last 100
	Seen []int `json:"-"`

	// LastSeen in a multicast announce
	LastSeen4 time.Time

	// IP4 address of last announce
	Last4 *net.UDPAddr `json:"-"`

	// LastSeen in a multicast announce
	LastSeen6 time.Time `json:"-"`

	// IP6 address of last announce
	Last6 *net.UDPAddr `json:"-"`

	// Number of multicast received
	Announces int

	// Numbers of announces received from that node on the P2P interface
	AnnouncesOnP2P int

	// Numbers of announces received from that node on the P2P interface
	AnnouncesFromP2P int

	// Information from the node - from an announce or message.
	NodeAnnounce *NodeAnnounce
}

type NodeGetter func(pub []byte) *DMNode

var (
	NodeF NodeGetter
)

func NewDMNode() *DMNode {
	now := time.Now()
	return &DMNode{
		Labels:       map[string]string{},
		FirstSeen:    now,
		LastSeen:     now,
		NodeAnnounce: &NodeAnnounce{},
	}
}

// Dial a stream over a multiplexed connection.
type TunDialer interface {
	// DialProxy will use the remote gateway to jump to
	// a different destination, indicated by stream.
	// On return, the stream ServerOut and ServerIn will be
	// populated, and connected to stream Dest.
	DialProxy(tp *Stream) error
}

// Used to avoid a direct dependency - for example in netstack.
// TcpProxy implements this interface.
type StreamProxy interface {
	Dial(destHost string, destAddr *net.TCPAddr) error
	Proxy() error
	Close()
}

// ClientDialer is implemented by streams like SSH
// client, that allow jumping to new destinations.
// Example SSHClientConn.
type ClientDialer interface {
	TunDialer

	// ForwardSocks opens a port on the gateway which will dynamically
	// connect to local destinations.
	ForwardSocks() error

	// ForwardTCP uses the gateway to forward 'remote' on the
	// gateway to a local port.
	ForwardTCP(local, remote string) error
}

// MUXDialer is implemented by a transport that can be
// used for egress for streams. SSHGate creating SSHClients is an example.
type MUXDialer interface {
	// Dial one TCP/mux connection to the IP:port.
	// The destination is a mesh node - port typically 5222, or 22 for 'regular' SSH serves.
	//
	// After handshake, an initial message is sent, including informations about the current node.
	//
	// The remote can be a trusted VPN, an untrusted AP/Gateway, a peer (link local or with public IP),
	// or a child. The subsriptions are used to indicate what messages will be forwarded to the server.
	// Typically VPN will receive all events, AP will receive subset of events related to topology while
	// child/peer only receive directed messages.
	DialMUX(addr string, pub []byte, subs []string) (ClientDialer, error)
}

// ReverseForwarder is used to tunnel accepted connections over a multiplexed stream.
// Implements -R in ssh.
// TODO: h2 implementation
// Used by acceptor.
type ReverseForwarder2 interface {
	ReverseForward2(in io.ReadCloser, out io.Writer, ip net.IP, port int, hostKey string, portKey uint32)
}

// IPResolver uses DNS cache or lookups to return the name
// associated with an IP, for metrics/stats/logs
type IPResolver interface {
	IPResolve(ip string) string
}

// Textual representation of the node registration data.
func (n *DMNode) String() string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Return the list of gateways for the node, starting with the link local if any.
func (n *DMNode) GWs() []*net.UDPAddr {
	res := []*net.UDPAddr{}

	if n.GW != nil {
		res = append(res, n.GW)
	}
	if n.Last4 != nil {
		res = append(res, n.Last4)
	}
	if n.Last6 != nil {
		res = append(res, n.Last6)
	}
	return res
}

// Called when receiving a registration or regular valid message via a different gateway.
// - HandleRegistrationRequest - after validating the VIP
//
//
// For VPN, the srcPort is assigned by the NAT, can be anything
// For direct, the port will be 5228 or 5229
func (n *DMNode) UpdateGWDirect(addr net.IP, zone string, srcPort int, onRes bool) {
	n.LastSeen = time.Now()
	n.GW = &net.UDPAddr{IP: addr, Port: srcPort, Zone: zone}
}
func (n *DMNode) BackoffReset() {
	n.Bacokff = 0
}
func (n *DMNode) BackoffSleep() {
	if n.Bacokff == 0 {
		n.Bacokff = 5 * time.Second
	}
	time.Sleep(n.Bacokff)
	if n.Bacokff < 5*time.Minute {
		n.Bacokff = n.Bacokff * 2
	}
}

// Track one interface.
type ActiveInterface struct {
	// Interface name. Name containing 'p2p' results in specific behavior.
	Name string

	// IP6 link local address. May be nil if IPPub is set.
	// One or the other must be set.
	IP6LL net.IP

	// IP4 address - may be a routable address, nil or private address.
	// If public address - may be included in the register, but typically not
	// useful.
	IP4 net.IP

	// Public addresses. IP6 address may be used for direct connections (in some
	// cases)
	IPPub []net.IP

	// Port for the UDP unicast link-local listener.
	Port int
	// Port for the UDP unicast link-local listener.
	Port4 int

	// True if this interface is an Android AP
	AndroidAP bool

	// True if this interface is connected to an Android DM node.
	AndroidAPClient bool
}