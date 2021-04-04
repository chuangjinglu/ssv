package p2p

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	gcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	iaddr "github.com/ipfs/go-ipfs-addr"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	noise "github.com/libp2p/go-libp2p-noise"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	mdnsDiscover "github.com/libp2p/go-libp2p/p2p/discovery"
	"github.com/libp2p/go-tcp-transport"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/prysmaticlabs/go-bitfield"
	"github.com/prysmaticlabs/prysm/shared/featureconfig"
	"github.com/prysmaticlabs/prysm/shared/fileutil"
	"github.com/prysmaticlabs/prysm/shared/iputils"
	"github.com/prysmaticlabs/prysm/shared/version"
	"go.opencensus.io/trace"
	"log"
	"net"
	"path/filepath"
	"runtime"
	"time"

	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// discoveryNotifee gets notified when we find a new peer via mDNS discovery
type discoveryNotifee struct {
	host   host.Host
	logger *zap.Logger
}

type Listener interface {
	Self() *enode.Node
	Close()
	Lookup(enode.ID) []*enode.Node
	Resolve(*enode.Node) *enode.Node
	RandomNodes() enode.Iterator
	Ping(*enode.Node) error
	RequestENR(*enode.Node) (*enode.Node, error)
	LocalNode() *enode.LocalNode
}

// HandlePeerFound connects to peers discovered via mDNS. Once they're connected,
// the PubSub system will automatically start interacting with them if they also
// support PubSub.
func (n *discoveryNotifee) HandlePeerFound(pi peer.AddrInfo) {
	err := n.host.Connect(context.Background(), pi)
	if err != nil {
		n.logger.Error("error connecting to peer", zap.String("peer_id", pi.ID.Pretty()), zap.Error(err))
	}
}

// setupDiscovery creates an mDNS discovery service and attaches it to the libp2p Host.
// This lets us automatically discover peers on the same LAN and connect to them.
func setupDiscovery(ctx context.Context, logger *zap.Logger, host host.Host) error {
	disc, err := mdnsDiscover.NewMdnsService(ctx, host, DiscoveryInterval, DiscoveryServiceTag)
	if err != nil {
		return errors.Wrap(err, "failed to create new mDNS service")
	}

	disc.RegisterNotifee(&discoveryNotifee{
		host:   host,
		logger: logger,
	})

	return nil
}

func parseBootStrapAddrs(addrs []string) (discv5Nodes []string) {
	discv5Nodes, _ = parseGenericAddrs(addrs)
	if len(discv5Nodes) == 0 {
		log.Print("No bootstrap addresses supplied")
	}
	return discv5Nodes
}

func parseGenericAddrs(addrs []string) (enodeString, multiAddrString []string) {
	for _, addr := range addrs {
		if addr == "" {
			// Ignore empty entries
			continue
		}
		_, err := enode.Parse(enode.ValidSchemes, addr)
		if err == nil {
			enodeString = append(enodeString, addr)
			continue
		}
		_, err = multiAddrFromString(addr)
		if err == nil {
			multiAddrString = append(multiAddrString, addr)
			continue
		}
		//logger.Error("Invalid address of %s provided: %v", addr, err)
	}
	return enodeString, multiAddrString
}

func multiAddrFromString(address string) (ma.Multiaddr, error) {
	addr, err := iaddr.ParseString(address)
	if err != nil {
		return nil, err
	}
	return addr.Multiaddr(), nil
}

// Retrieves an external ipv4 address and converts into a libp2p formatted value.
func ipAddr() net.IP {
	ip, err := iputils.ExternalIP()
	if err != nil {
		log.Fatalf("Could not get IPv4 address: %v", err)
	}
	return net.ParseIP(ip)
}

// Determines a private key for p2p networking from the p2p service's
// configuration struct. If no key is found, it generates a new one.
func privKey() (*ecdsa.PrivateKey, error) {
	defaultKeyPath := DefaultDataDir()

	priv, _, err := crypto.GenerateSecp256k1Key(rand.Reader)
	if err != nil {
		return nil, err
	}
	rawbytes, err := priv.Raw()
	if err != nil {
		return nil, err
	}
	dst := make([]byte, hex.EncodedLen(len(rawbytes)))
	hex.Encode(dst, rawbytes)
	if err := fileutil.WriteFile(defaultKeyPath, dst); err != nil {
		return nil, err
	}
	convertedKey := convertFromInterfacePrivKey(priv)
	return convertedKey, nil
}

// DefaultDataDir is the default data directory to use for the databases and other
// persistence requirements.

// buildOptions for the libp2p host.
func (n *p2pNetwork) buildOptions(ip net.IP, priKey *ecdsa.PrivateKey) []libp2p.Option {
	//cfg := s.cfg
	listen, err := multiAddressBuilder(ip.String(), uint(n.cfg.TcpPort))
	if err != nil {
		log.Fatalf("Failed to p2p listen: %v", err)
	}
	//if cfg.LocalIP != "" {
	//	if net.ParseIP(cfg.LocalIP) == nil {
	//		log.Fatalf("Invalid Local ip provided: %s", cfg.LocalIP)
	//	}
	//	listen, err = multiAddressBuilder(cfg.LocalIP, cfg.TCPPort)
	//	if err != nil {
	//		log.Fatalf("Failed to p2p listen: %v", err)
	//	}
	//}
	options := []libp2p.Option{
		privKeyOption(priKey),
		libp2p.ListenAddrs(listen),
		libp2p.UserAgent(version.GetBuildData()),
		// TODO
		//libp2p.ConnectionGater(&prysmP2pService.Service{}),
		libp2p.Transport(tcp.NewTCPTransport),
	}

	options = append(options, libp2p.Security(noise.ID, noise.New))

	//if cfg.EnableUPnP {
	//	options = append(options, libp2p.NATPortMap()) // Allow to use UPnP
	//}
	//if cfg.RelayNodeAddr != "" {
	//	options = append(options, libp2p.AddrsFactory(withRelayAddrs(cfg.RelayNodeAddr)))
	//} else {
	// Disable relay if it has not been set.
	options = append(options, libp2p.DisableRelay())
	//}
	//if cfg.HostAddress != "" {
	//	options = append(options, libp2p.AddrsFactory(func(addrs []ma.Multiaddr) []ma.Multiaddr {
	//		external, err := multiAddressBuilder(cfg.HostAddress, cfg.TCPPort)
	//		if err != nil {
	//			log.WithError(err).Error("Unable to create external multiaddress")
	//		} else {
	//			addrs = append(addrs, external)
	//		}
	//		return addrs
	//	}))
	//}
	//if cfg.HostDNS != "" {
	//	options = append(options, libp2p.AddrsFactory(func(addrs []ma.Multiaddr) []ma.Multiaddr {
	//		external, err := ma.NewMultiaddr(fmt.Sprintf("/dns4/%s/tcp/%d", cfg.HostDNS, cfg.TCPPort))
	//		if err != nil {
	//			log.WithError(err).Error("Unable to create external multiaddress")
	//		} else {
	//			addrs = append(addrs, external)
	//		}
	//		return addrs
	//	}))
	//}
	// Disable Ping Service.
	options = append(options, libp2p.Ping(false))
	return options
}

func (n *p2pNetwork) startDiscoveryV5(addr net.IP, privKey *ecdsa.PrivateKey) (*discover.UDPv5, error) {
	listener, err := n.createListener(addr, privKey)
	if err != nil {
		return nil, errors.Wrap(err, "could not create listener")
	}
	record := listener.Self()
	log.Print("ENR: ", record.String())
	return listener, nil
}

func (n *p2pNetwork) connectToBootnodes() error {
	nodes := make([]*enode.Node, 0, len(n.cfg.Discv5BootStrapAddr))
	for _, addr := range n.cfg.Discv5BootStrapAddr {
		bootNode, err := enode.Parse(enode.ValidSchemes, addr)
		if err != nil {
			return err
		}
		// do not dial bootnodes with their tcp ports not set
		if err := bootNode.Record().Load(enr.WithEntry("tcp", new(enr.TCP))); err != nil {
			if !enr.IsNotFound(err) {
				log.Print("Could not retrieve tcp port")
				log.Print(err)
				//log.WithError(err).Error("Could not retrieve tcp port")
			}
			continue
		}
		nodes = append(nodes, bootNode)
	}
	multiAddresses := convertToMultiAddr(nodes)
	n.connectWithAllPeers(multiAddresses)
	return nil
}

func (n *p2pNetwork) createListener(ipAddr net.IP, privKey *ecdsa.PrivateKey) (*discover.UDPv5, error) {
	// BindIP is used to specify the ip
	// on which we will bind our listener on
	// by default we will listen to all interfaces.
	var bindIP net.IP
	switch udpVersionFromIP(ipAddr) {
	case "udp4":
		bindIP = net.IPv4zero
	case "udp6":
		bindIP = net.IPv6zero
	default:
		return nil, errors.New("invalid ip provided")
	}

	//// If Local ip is specified then use that instead.
	//if s.cfg.LocalIP != "" {
	//	ipAddr = net.ParseIP(s.cfg.LocalIP)
	//	if ipAddr == nil {
	//		return nil, errors.New("invalid Local ip provided")
	//	}
	//	bindIP = ipAddr
	//}
	udpAddr := &net.UDPAddr{
		IP:   bindIP,
		Port: n.cfg.UdpPort,
	}
	// Listen to all network interfaces
	// for both ip protocols.
	networkVersion := "udp"
	conn, err := net.ListenUDP(networkVersion, udpAddr)
	if err != nil {
		return nil, errors.Wrap(err, "could not listen to UDP")
	}

	localNode, err := createLocalNode(
		privKey,
		ipAddr,
		n.cfg.UdpPort,
		n.cfg.TcpPort,
	)
	if err != nil {
		return nil, errors.Wrap(err, "could not create Local node")
	}
	if n.cfg.HostAddress != "" {
		hostIP := net.ParseIP(n.cfg.HostAddress)
		if hostIP.To4() == nil && hostIP.To16() == nil {
			log.Print("Invalid host address given: %s", hostIP.String())
		} else {
			localNode.SetFallbackIP(hostIP)
			localNode.SetStaticIP(hostIP)
		}
	}
	if n.cfg.HostDNS != "" {
		host := n.cfg.HostDNS
		ips, err := net.LookupIP(host)
		if err != nil {
			return nil, errors.Wrap(err, "could not resolve host address")
		}
		if len(ips) > 0 {
			// Use first IP returned from the
			// resolver.
			firstIP := ips[0]
			localNode.SetFallbackIP(firstIP)
		}
	}

	//if s.cfg.HostDNS != "" {
	//	host := s.cfg.HostDNS
	//	ips, err := net.LookupIP(host)
	//	if err != nil {
	//		return nil, errors.Wrap(err, "could not resolve host address")
	//	}
	//	if len(ips) > 0 {
	//		// Use first IP returned from the
	//		// resolver.
	//		firstIP := ips[0]
	//		localNode.SetFallbackIP(firstIP)
	//	}
	//}
	dv5Cfg := discover.Config{
		PrivateKey: privKey,
	}
	dv5Cfg.Bootnodes = []*enode.Node{}
	for _, addr := range n.cfg.Discv5BootStrapAddr {
		bootNode, err := enode.Parse(enode.ValidSchemes, addr)
		if err != nil {
			return nil, errors.Wrap(err, "could not bootstrap addr")
		}
		dv5Cfg.Bootnodes = append(dv5Cfg.Bootnodes, bootNode)
	}

	listener, err := discover.ListenV5(conn, localNode, dv5Cfg)
	if err != nil {
		return nil, errors.Wrap(err, "could not listen to discV5")
	}
	return listener, nil
}

func (n *p2pNetwork) connectWithAllPeers(multiAddrs []ma.Multiaddr) {
	addrInfos, err := peer.AddrInfosFromP2pAddrs(multiAddrs...)
	if err != nil {
		//log.Errorf("Could not convert to peer address info's from multiaddresses: %v", err)
		return
	}
	for _, info := range addrInfos {
		// make each dial non-blocking
		go func(info peer.AddrInfo) {
			if err := n.connectWithPeer(n.ctx, info); err != nil {
				//log.Print("Could not connect with peer ", info.String(), err)
				//log.WithError(err).Tracef("Could not connect with peer %s", info.String())
			}
		}(info)
	}
}

func (n *p2pNetwork) connectWithPeer(ctx context.Context, info peer.AddrInfo) error {
	ctx, span := trace.StartSpan(ctx, "p2p.connectWithPeer")
	defer span.End()

	if info.ID == n.host.ID() {
		return nil
	}
	if n.peers.IsBad(info.ID) {
		return errors.New("refused to connect to bad peer")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := n.host.Connect(ctx, info); err != nil {
		//s.Peers().Scorers().BadResponsesScorer().Increment(info.ID)
		//log.Print("TEST peer connect error ------------", err)
		return err
	}
	//log.Print("Connected to peer!!!!  ", info)
	return nil
}

// listen for new nodes watches for new nodes in the network and adds them to the peerstore.
func (n *p2pNetwork) listenForNewNodes() {
	iterator := n.dv5Listener.RandomNodes()
	//iterator = enode.Filter(iterator, s.filterPeer)
	defer iterator.Close()
	for {
		// Exit if service's context is canceled
		if n.ctx.Err() != nil {
			break
		}
		if n.isPeerAtLimit(false /* inbound */) {
			// Pause the main loop for a period to stop looking
			// for new peers.
			//log.Trace("Not looking for peers, at peer limit")
			time.Sleep(6 * time.Second)
			continue
		}
		exists := iterator.Next()
		if !exists {
			break
		}
		node := iterator.Node()
		peerInfo, _, err := convertToAddrInfo(node)
		if err != nil {
			//log.WithError(err).Error("Could not convert to peer info")
			continue
		}
		go func(info *peer.AddrInfo) {
			if err := n.connectWithPeer(n.ctx, *info); err != nil {
				//log.WithError(err).Tracef("Could not connect with peer %s", info.String())
				//log.Print(err)
			}
		}(peerInfo)
	}
}

// This checks our set max peers in our config, and
// determines whether our currently connected and
// active peers are above our set max peer limit.
func (n *p2pNetwork) isPeerAtLimit(inbound bool) bool {
	numOfConns := len(n.host.Network().Peers())
	maxPeers := 45
	// If we are measuring the limit for inbound peers
	// we apply the high watermark buffer.
	//if inbound {
	//	maxPeers += highWatermarkBuffer
	//	maxInbound := s.peers.InboundLimit() + highWatermarkBuffer
	//	currInbound := len(s.peers.InboundConnected())
	//	// Exit early if we are at the inbound limit.
	//	if currInbound >= maxInbound {
	//		return true
	//	}
	//}
	activePeers := len(n.peers.Active())
	return activePeers >= maxPeers || numOfConns >= maxPeers
}

func udpVersionFromIP(ipAddr net.IP) string {
	if ipAddr.To4() != nil {
		return "udp4"
	}
	return "udp6"
}

func createLocalNode(privKey *ecdsa.PrivateKey, ipAddr net.IP, udpPort, tcpPort int) (*enode.LocalNode, error) {
	db, err := enode.OpenDB("")
	if err != nil {
		return nil, errors.Wrap(err, "could not open node's peer database")
	}
	localNode := enode.NewLocalNode(db, privKey)

	ipEntry := enr.IP(ipAddr)
	udpEntry := enr.UDP(udpPort)
	tcpEntry := enr.TCP(tcpPort)
	localNode.Set(ipEntry)
	localNode.Set(udpEntry)
	localNode.Set(tcpEntry)
	localNode.SetFallbackIP(ipAddr)
	localNode.SetFallbackUDP(udpPort)

	//localNode, err = addForkEntry(localNode, s.genesisTime, s.genesisValidatorsRoot)
	//if err != nil {
	//	return nil, errors.Wrap(err, "could not add eth2 fork version entry to enr")
	//}
	return intializeAttSubnets(localNode), nil
}

// Initializes a bitvector of attestation subnets beacon nodes is subscribed to
// and creates a new ENR entry with its default value.
func intializeAttSubnets(node *enode.LocalNode) *enode.LocalNode {
	bitV := bitfield.NewBitvector64()
	entry := enr.WithEntry("attnets", bitV.Bytes())
	node.Set(entry)
	return node
}

func convertToMultiAddr(nodes []*enode.Node) []ma.Multiaddr {
	var multiAddrs []ma.Multiaddr
	for _, node := range nodes {
		// ignore nodes with no ip address stored
		if node.IP() == nil {
			continue
		}
		multiAddr, err := convertToSingleMultiAddr(node)
		if err != nil {
			//log.WithError(err).Error("Could not convert to multiAddr")
			continue
		}
		multiAddrs = append(multiAddrs, multiAddr)
	}
	return multiAddrs
}

func convertToSingleMultiAddr(node *enode.Node) (ma.Multiaddr, error) {
	pubkey := node.Pubkey()
	assertedKey := convertToInterfacePubkey(pubkey)
	id, err := peer.IDFromPublicKey(assertedKey)
	if err != nil {
		return nil, errors.Wrap(err, "could not get peer id")
	}
	return multiAddressBuilderWithID(node.IP().String(), "tcp", uint(node.TCP()), id)
}

func convertToInterfacePubkey(pubkey *ecdsa.PublicKey) crypto.PubKey {
	typeAssertedKey := crypto.PubKey((*crypto.Secp256k1PublicKey)(pubkey))
	return typeAssertedKey
}

func multiAddressBuilderWithID(ipAddr, protocol string, port uint, id peer.ID) (ma.Multiaddr, error) {
	parsedIP := net.ParseIP(ipAddr)
	if parsedIP.To4() == nil && parsedIP.To16() == nil {
		return nil, errors.Errorf("invalid ip address provided: %s", ipAddr)
	}
	if id.String() == "" {
		return nil, errors.New("empty peer id given")
	}
	if parsedIP.To4() != nil {
		return ma.NewMultiaddr(fmt.Sprintf("/ip4/%s/%s/%d/p2p/%s", ipAddr, protocol, port, id.String()))
	}
	return ma.NewMultiaddr(fmt.Sprintf("/ip6/%s/%s/%d/p2p/%s", ipAddr, protocol, port, id.String()))
}

func multiAddressBuilder(ipAddr string, tcpPort uint) (ma.Multiaddr, error) {
	parsedIP := net.ParseIP(ipAddr)
	if parsedIP.To4() == nil && parsedIP.To16() == nil {
		return nil, errors.Errorf("invalid ip address provided: %s", ipAddr)
	}
	if parsedIP.To4() != nil {
		return ma.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/%d", ipAddr, tcpPort))
	}
	return ma.NewMultiaddr(fmt.Sprintf("/ip6/%s/tcp/%d", ipAddr, tcpPort))
}

// Adds a private key to the libp2p option if the option was provided.
// If the private key file is missing or cannot be read, or if the
// private key contents cannot be marshaled, an exception is thrown.
func privKeyOption(privkey *ecdsa.PrivateKey) libp2p.Option {
	return func(cfg *libp2p.Config) error {
		//log.Debug("ECDSA private key generated")
		return cfg.Apply(libp2p.Identity(convertToInterfacePrivkey(privkey)))
	}
}

func convertToInterfacePrivkey(privkey *ecdsa.PrivateKey) crypto.PrivKey {
	typeAssertedKey := crypto.PrivKey((*crypto.Secp256k1PrivateKey)(privkey))
	return typeAssertedKey
}

func setPubSubParameters() {
	heartBeatInterval := 700 * time.Millisecond
	pubsub.GossipSubDlo = 6
	pubsub.GossipSubD = 8
	pubsub.GossipSubHeartbeatInterval = heartBeatInterval
	pubsub.GossipSubHistoryLength = 6
	pubsub.GossipSubHistoryGossip = 3
	pubsub.TimeCacheDuration = 550 * heartBeatInterval

	// Set a larger gossip history to ensure that slower
	// messages have a longer time to be propagated. This
	// comes with the tradeoff of larger memory usage and
	// size of the seen message cache.
	if featureconfig.Get().EnableLargerGossipHistory {
		pubsub.GossipSubHistoryLength = 12
		pubsub.GossipSubHistoryLength = 5
	}
}

func convertToAddrInfo(node *enode.Node) (*peer.AddrInfo, ma.Multiaddr, error) {
	multiAddr, err := convertToSingleMultiAddr(node)
	if err != nil {
		return nil, nil, err
	}
	info, err := peer.AddrInfoFromP2pAddr(multiAddr)
	if err != nil {
		return nil, nil, err
	}
	return info, multiAddr, nil
}

func DefaultDataDir() string {
	// Try to place the data folder in the user's home dir
	home := fileutil.HomeDir()
	if home != "" {
		if runtime.GOOS == "darwin" {
			return filepath.Join(home, "Library", "Eth2")
		} else if runtime.GOOS == "windows" {
			return filepath.Join(home, "AppData", "Local", "Eth2")
		} else {
			return filepath.Join(home, ".eth2")
		}
	}
	// As we cannot guess a stable location, return empty and handle later
	return ""
}

func convertFromInterfacePrivKey(privkey crypto.PrivKey) *ecdsa.PrivateKey {
	typeAssertedKey := (*ecdsa.PrivateKey)(privkey.(*crypto.Secp256k1PrivateKey))
	typeAssertedKey.Curve = gcrypto.S256() // Temporary hack, so libp2p Secp256k1 is recognized as geth Secp256k1 in disc v5.1.
	return typeAssertedKey
}