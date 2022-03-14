package v1

import (
	"context"
	"github.com/bloxapp/ssv/ibft/proto"
	"github.com/bloxapp/ssv/network"
	"github.com/bloxapp/ssv/network/commons/listeners"
	"github.com/bloxapp/ssv/network/forks"
	streams_v0 "github.com/bloxapp/ssv/network/p2p/streams"
	p2p_v1 "github.com/bloxapp/ssv/network/p2p_v1"
	"github.com/bloxapp/ssv/network/p2p_v1/adapter"
	"github.com/bloxapp/ssv/network/p2p_v1/discovery"
	"github.com/bloxapp/ssv/network/p2p_v1/peers"
	"github.com/bloxapp/ssv/network/p2p_v1/streams"
	"github.com/bloxapp/ssv/network/p2p_v1/topics"
	"github.com/bloxapp/ssv/utils/tasks"
	"github.com/herumi/bls-eth-go-binary/bls"
	core "github.com/libp2p/go-libp2p-core"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/pkg/errors"
	"github.com/prysmaticlabs/prysm/async"
	"go.uber.org/zap"
	"time"
)

const (
	decidedTopic = "decided"

	legacyMsgStream = "/sync/0.0.1"
)

// netV0Adapter is an adapter for network v0
type netV0Adapter struct {
	ctx    context.Context
	cancel context.CancelFunc

	logger *zap.Logger

	v1Cfg        *p2p_v1.Config
	fork         forks.Fork
	host         host.Host
	streamCtrlv0 streams_v0.StreamController

	streamCtrl streams.StreamController

	topicsCtrl topics.Controller
	idx        peers.Index
	disc       discovery.Service
	listeners  listeners.Container
}

// NewV0Adapter creates a new v0 network with underlying v1 infra
func New(pctx context.Context, v1Cfg *p2p_v1.Config) adapter.Adapter {
	// TODO: ensure that the old user agent is passed in v1Cfg.UserAgent
	ctx, cancel := context.WithCancel(pctx)
	return &netV0Adapter{
		ctx:       ctx,
		cancel:    cancel,
		logger:    v1Cfg.Logger,
		listeners: listeners.NewListenersContainer(pctx, v1Cfg.Logger),
	}
}

func (n *netV0Adapter) Listeners() listeners.Container {
	return n.listeners
}

// Setup initializes all required components
func (n *netV0Adapter) Setup() error {
	n.setLegacyStreamHandler()

	if err := n.setupHost(); err != nil {
		return errors.Wrap(err, "could not setup libp2p host")
	}
	// creating 2 stream controllers, first for supporting old sync and new for handshake
	n.streamCtrlv0 = streams_v0.NewStreamController(n.ctx, n.logger, n.host, n.fork, n.v1Cfg.RequestTimeout)
	n.streamCtrl = streams.NewStreamController(n.ctx, n.logger, n.host, n.fork, n.v1Cfg.RequestTimeout)

	if err := n.setupPeerServices(); err != nil {
		return errors.Wrap(err, "could not setup peers discovery")
	}
	if err := n.setupDiscovery(); err != nil {
		return errors.Wrap(err, "could not bootstrap discovery")
	}
	if err := n.setupPubsub(); err != nil {
		return errors.Wrap(err, "could not setup pubsub")
	}

	return nil
}

// Start starts the network
func (n *netV0Adapter) Start() error {
	go func() {
		err := tasks.Retry(func() error {
			return n.disc.Bootstrap(func(e discovery.PeerEvent) {
				// TODO: check if relevant
				if err := n.host.Connect(n.ctx, e.AddrInfo); err != nil {
					n.logger.Warn("could not connect peer",
						zap.String("peer", e.AddrInfo.String()), zap.Error(err))
					return
				}
				n.logger.Debug("connected peer",
					zap.String("peer", e.AddrInfo.String()))
			})
		}, 3)
		if err != nil {
			n.logger.Panic("could not bootstrap discovery", zap.Error(err))
		}
	}()

	async.RunEvery(n.ctx, 15*time.Minute, func() {
		n.idx.GC()
	})

	async.RunEvery(n.ctx, 30*time.Second, func() {
		go n.reportAllPeers()
		n.reportTopics()
	})

	return nil
}

// Close closes the network
func (n *netV0Adapter) Close() error {
	n.cancel()
	if err := n.idx.Close(); err != nil {
		n.logger.Error("could not close index", zap.Error(err))
	}
	return n.host.Close()
}

// HandleMsg implements topics.PubsubMessageHandler
func (n *netV0Adapter) HandleMsg(topic string, msg *pubsub.Message) error {
	cm, err := n.fork.DecodeNetworkMsg(msg.Data)
	if err != nil {
		return err
	}

	if cm == nil || cm.SignedMessage == nil {
		n.logger.Debug("could not propagate nil message")
		return nil
	}

	n.propagateSignedMsg(cm)

	return nil
}

func (n *netV0Adapter) ReceivedMsgChan() (<-chan *proto.SignedMessage, func()) {
	ls := listeners.NewListener(network.NetworkMsg_IBFTType)

	return ls.MsgChan(), n.listeners.Register(ls)
}

func (n *netV0Adapter) ReceivedSignatureChan() (<-chan *proto.SignedMessage, func()) {
	ls := listeners.NewListener(network.NetworkMsg_SignatureType)

	return ls.SigChan(), n.listeners.Register(ls)
}

func (n *netV0Adapter) ReceivedDecidedChan() (<-chan *proto.SignedMessage, func()) {
	ls := listeners.NewListener(network.NetworkMsg_DecidedType)

	return ls.DecidedChan(), n.listeners.Register(ls)
}

func (n *netV0Adapter) ReceivedSyncMsgChan() (<-chan *network.SyncChanObj, func()) {
	ls := listeners.NewListener(network.NetworkMsg_SyncType)

	return ls.SyncChan(), n.listeners.Register(ls)
}

func (n *netV0Adapter) SubscribeToValidatorNetwork(validatorPk *bls.PublicKey) error {
	topic := n.v1Cfg.Fork.ValidatorTopicID(validatorPk.Serialize())
	return n.topicsCtrl.Subscribe(topic)
}

func (n *netV0Adapter) AllPeers(validatorPk []byte) ([]string, error) {
	topic := n.v1Cfg.Fork.ValidatorTopicID(validatorPk)
	peers, err := n.topicsCtrl.Peers(topic)
	if err != nil {
		return nil, err
	}
	var results []string
	for _, p := range peers {
		pid := p.String()
		if pid == n.v0Cfg.ExporterPeerID {
			continue
		}
		results = append(results, p.String())
	}
	return results, nil
}

func (n *netV0Adapter) SubscribeToMainTopic() error {
	return n.topicsCtrl.Subscribe(decidedTopic)
}

func (n *netV0Adapter) MaxBatch() uint64 {
	return n.v0Cfg.MaxBatchResponse
}

func (n *netV0Adapter) Broadcast(validatorPK []byte, msg *proto.SignedMessage) error {
	msgBytes, err := n.fork.EncodeNetworkMsg(&network.Message{
		SignedMessage: msg,
		Type:          network.NetworkMsg_IBFTType,
	})
	if err != nil {
		return errors.Wrap(err, "failed to marshal message")
	}
	topic := n.fork.ValidatorTopicID(validatorPK)
	if err := n.topicsCtrl.Broadcast(topic, msgBytes, time.Second*8); err != nil {
		return errors.Wrap(err, "could not broadcast signature")
	}
	return nil
}

func (n *netV0Adapter) BroadcastSignature(validatorPK []byte, msg *proto.SignedMessage) error {
	msgBytes, err := n.fork.EncodeNetworkMsg(&network.Message{
		SignedMessage: msg,
		Type:          network.NetworkMsg_SignatureType,
	})
	if err != nil {
		return errors.Wrap(err, "failed to marshal message")
	}
	topic := n.fork.ValidatorTopicID(validatorPK)
	if err := n.topicsCtrl.Broadcast(topic, msgBytes, time.Second*8); err != nil {
		return errors.Wrap(err, "could not broadcast signature")
	}
	return nil
}

func (n *netV0Adapter) BroadcastDecided(validatorPK []byte, msg *proto.SignedMessage) error {
	msgBytes, err := n.fork.EncodeNetworkMsg(&network.Message{
		SignedMessage: msg,
		Type:          network.NetworkMsg_DecidedType,
	})
	if err != nil {
		return errors.Wrap(err, "failed to marshal message")
	}
	topic := n.fork.ValidatorTopicID(validatorPK)
	go func() {
		if err := n.topicsCtrl.Broadcast(topic, msgBytes, time.Second*10); err != nil {
			n.logger.Error("could not broadcast message on decided topic", zap.Error(err))
		}
	}()
	if err := n.topicsCtrl.Broadcast(topic, msgBytes, time.Second*8); err != nil {
		return errors.Wrap(err, "could not broadcast decided message")
	}
	return nil
}

func (n *netV0Adapter) GetHighestDecidedInstance(peerStr string, msg *network.SyncMessage) (*network.SyncMessage, error) {
	return n.sendSyncRequest(peerStr, msg)
}

func (n *netV0Adapter) GetDecidedByRange(peerStr string, msg *network.SyncMessage) (*network.SyncMessage, error) {
	return n.sendSyncRequest(peerStr, msg)
}

func (n *netV0Adapter) GetLastChangeRoundMsg(peerStr string, msg *network.SyncMessage) (*network.SyncMessage, error) {
	return n.sendSyncRequest(peerStr, msg)
}

func (n *netV0Adapter) RespondSyncMsg(streamID string, msg *network.SyncMessage) error {
	msg.FromPeerID = n.host.ID().Pretty()
	return n.streamCtrlv0.Respond(&network.Message{
		SyncMessage: msg,
		Type:        network.NetworkMsg_SyncType,
		StreamID:    streamID,
	})
}

func (n *netV0Adapter) NotifyOperatorID(oid string) {
	// TODO
	panic("implement me")
}

func (n *netV0Adapter) sendSyncRequest(peerStr string, msg *network.SyncMessage) (*network.SyncMessage, error) {
	pi, err := peer.Decode(peerStr)
	if err != nil {
		return nil, err
	}
	res, err := n.streamCtrlv0.Request(pi, legacyMsgStream, &network.Message{
		SyncMessage: msg,
		Type:        network.NetworkMsg_SyncType,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to make sync request")
	}
	if res.SyncMessage == nil {
		return nil, errors.New("no response for sync request")
	}
	n.logger.Debug("got sync response",
		zap.String("FromPeerID", res.SyncMessage.GetFromPeerID()))
	return res.SyncMessage, nil
}

func (n *netV0Adapter) setLegacyStreamHandler() {
	n.host.SetStreamHandler("/sync/0.0.1", func(stream core.Stream) {
		cm, _, err := n.streamCtrlv0.HandleStream(stream)
		if err != nil {
			n.logger.Error(" highest decided preStreamHandler failed", zap.Error(err))
			return
		}
		if cm == nil {
			n.logger.Debug("got nil sync message")
			return
		}
		// adjusting message and propagating to other (internal) components
		cm.SyncMessage.FromPeerID = stream.Conn().RemotePeer().String()
		go propagateSyncMessage(n.listeners.GetListeners(network.NetworkMsg_SyncType), cm)
	})
}
