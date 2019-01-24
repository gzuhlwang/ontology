package chainmgr

import (
	"bytes"
	"fmt"
	"math"
	"reflect"
	"sync"
	"time"

	"encoding/hex"
	"github.com/ontio/ontology-crypto/keypair"
	"github.com/ontio/ontology-eventbus/actor"
	"github.com/ontio/ontology-eventbus/eventstream"
	"github.com/ontio/ontology-eventbus/remote"
	"github.com/ontio/ontology/account"
	"github.com/ontio/ontology/common/config"
	"github.com/ontio/ontology/common/log"
	shardmsg "github.com/ontio/ontology/core/chainmgr/message"
	"github.com/ontio/ontology/core/ledger"
	"github.com/ontio/ontology/core/types"
	"github.com/ontio/ontology/events"
	"github.com/ontio/ontology/events/message"
	"github.com/ontio/ontology/smartcontract/service/native/shardmgmt/states"
)

const (
	CAP_LOCAL_SHARDMSG_CHNL  = 64
	CAP_REMOTE_SHARDMSG_CHNL = 64
	CAP_SHARD_BLOCK_POOL     = 16
	CONNECT_PARENT_TIMEOUT   = 5 * time.Second
)

var defaultChainManager *ChainManager = nil

type RemoteMsg struct {
	Sender *actor.PID
	Msg    shardmsg.RemoteShardMsg
}

type MsgSendReq struct {
	targetShardID uint64
	msg           *shardmsg.CrossShardMsg
}

type ShardInfo struct {
	ShardAddress string
	Connected    bool
	Config       *config.OntologyConfig
	Sender       *actor.PID
}

type ChainManager struct {
	shardID              uint64
	shardPort            uint
	parentShardID        uint64
	parentShardIPAddress string
	parentShardPort      uint

	lock       sync.RWMutex
	shards     map[uint64]*ShardInfo
	shardAddrs map[string]uint64
	blockPool  *shardmsg.ShardBlockPool

	account      *account.Account
	genesisBlock *types.Block

	ledger *ledger.Ledger
	p2pPid *actor.PID

	localBlockMsgC  chan *types.Block
	localEventC     chan *shardstates.ShardEventState
	remoteShardMsgC chan *RemoteMsg
	broadcastMsgC   chan *MsgSendReq
	parentConnWait  chan bool

	parentPid   *actor.PID
	localPid    *actor.PID
	sub         *events.ActorSubscriber
	endpointSub *eventstream.Subscription

	quitC  chan struct{}
	quitWg sync.WaitGroup
}

func Initialize(shardID, parentShardID uint64, parentAddr string, shardPort, parentPort uint, acc *account.Account) (*ChainManager, error) {
	// fixme: change to sync.once
	if defaultChainManager != nil {
		return nil, fmt.Errorf("chain manager had been initialized for shard: %d", defaultChainManager.shardID)
	}

	blkPool := shardmsg.NewShardBlockPool(CAP_SHARD_BLOCK_POOL)
	if blkPool == nil {
		return nil, fmt.Errorf("chainmgr init: failed to init block pool")
	}

	chainMgr := &ChainManager{
		shardID:              shardID,
		shardPort:            shardPort,
		parentShardID:        parentShardID,
		parentShardIPAddress: parentAddr,
		parentShardPort:      parentPort,
		shards:               make(map[uint64]*ShardInfo),
		shardAddrs:           make(map[string]uint64),
		blockPool:            blkPool,
		localBlockMsgC:       make(chan *types.Block, CAP_LOCAL_SHARDMSG_CHNL),
		localEventC:          make(chan *shardstates.ShardEventState, CAP_LOCAL_SHARDMSG_CHNL),
		remoteShardMsgC:      make(chan *RemoteMsg, CAP_REMOTE_SHARDMSG_CHNL),
		broadcastMsgC:        make(chan *MsgSendReq, CAP_REMOTE_SHARDMSG_CHNL),
		parentConnWait:       make(chan bool),
		quitC:                make(chan struct{}),

		account: acc,
	}

	chainMgr.startRemoteEventbus()
	if err := chainMgr.startListener(); err != nil {
		return nil, fmt.Errorf("shard %d start listener failed: %s", chainMgr.shardID, err)
	}

	go chainMgr.localEventLoop()
	go chainMgr.remoteShardMsgLoop()
	go chainMgr.broadcastMsgLoop()

	if err := chainMgr.connectParent(); err != nil {
		chainMgr.Stop()
		return nil, fmt.Errorf("connect parent shard failed: %s", err)
	}

	chainMgr.endpointSub = eventstream.Subscribe(chainMgr.remoteEndpointEvent).
		WithPredicate(func(m interface{}) bool {
			switch m.(type) {
			case *remote.EndpointTerminatedEvent:
				return true
			default:
				return false
			}
		})

	defaultChainManager = chainMgr
	return defaultChainManager, nil
}

func (self *ChainManager) SetP2P(p2p *actor.PID) error {
	if defaultChainManager == nil {
		return fmt.Errorf("uninitialized chain manager")
	}

	defaultChainManager.p2pPid = p2p
	return nil
}

func (self *ChainManager) LoadFromLedger(lgr *ledger.Ledger) error {
	// TODO: get all shards from local ledger

	self.ledger = lgr

	// start listen on local actor
	self.sub = events.NewActorSubscriber(self.localPid)
	self.sub.Subscribe(message.TOPIC_SHARD_SYSTEM_EVENT)
	self.sub.Subscribe(message.TOPIC_SAVE_BLOCK_COMPLETE)

	globalState, err := self.getShardMgmtGlobalState()
	if err != nil {

	}
	if globalState == nil {
		// not initialized from ledger
		log.Info("chainmgr: shard-mgmt not initialized, skipped loading from ledger")
		return nil
	}

	peerPK := hex.EncodeToString(keypair.SerializePublicKey(self.account.PublicKey))

	for i := uint64(1); i < globalState.NextShardID; i++ {
		shard, err := self.getShardState(i)
		if err != nil {
			log.Errorf("get shard %d failed: %s", i, err)
		}
		if shard.State != shardstates.SHARD_STATE_ACTIVE {
			continue
		}
		if _, present := shard.Peers[peerPK]; present {
			// peer is in the shard
			// build shard config
		}
	}

	return nil
}

func (self *ChainManager) startRemoteEventbus() {
	localRemote := fmt.Sprintf("%s:%d", config.DEFAULT_PARENTSHARD_IPADDR, self.shardPort)
	remote.Start(localRemote)
}

func (self *ChainManager) Receive(context actor.Context) {
	switch msg := context.Message().(type) {
	case *actor.Restarting:
		log.Info("chain mgr actor restarting")
	case *actor.Stopping:
		log.Info("chain mgr actor stopping")
	case *actor.Stopped:
		log.Info("chain mgr actor stopped")
	case *actor.Started:
		log.Info("chain mgr actor started")
	case *actor.Restart:
		log.Info("chain mgr actor restart")
	case *message.ShardSystemEventMsg:
		if msg == nil {
			return
		}
		evt := msg.Event
		log.Infof("chain mgr received shard system event: ver: %d, type: %d", evt.Version, evt.EventType)
		self.localEventC <- evt
	case *shardmsg.CrossShardMsg:
		if msg == nil {
			return
		}
		log.Tracef("chain mgr received shard msg: %v", msg)
		smsg, err := shardmsg.DecodeShardMsg(msg.Type, msg.Data)
		if err != nil {
			log.Errorf("decode shard msg: %s", err)
			return
		}
		self.remoteShardMsgC <- &RemoteMsg{
			Sender: msg.Sender,
			Msg:    smsg,
		}

	case *message.SaveBlockCompleteMsg:
		self.localBlockMsgC <- msg.Block

	default:
		log.Info("chain mgr actor: Unknown msg ", msg, "type", reflect.TypeOf(msg))
	}
}

func (self *ChainManager) remoteEndpointEvent(evt interface{}) {
	switch evt := evt.(type) {
	case *remote.EndpointTerminatedEvent:
		self.remoteShardMsgC <- &RemoteMsg{
			Msg: &shardmsg.ShardDisconnectedMsg{
				Address: evt.Address,
			},
		}
	default:
		return
	}
}

func (self *ChainManager) localEventLoop() error {

	self.quitWg.Add(1)
	defer self.quitWg.Done()

	for {
		select {
		case shardEvt := <-self.localEventC:
			switch shardEvt.EventType {
			case shardstates.EVENT_SHARD_CREATE:
				createEvt := &shardstates.CreateShardEvent{}
				if err := createEvt.Deserialize(bytes.NewBuffer(shardEvt.Payload)); err != nil {
					return fmt.Errorf("deserialize create shard event: %s", err)
				}
				if err := self.onShardCreated(createEvt); err != nil {
					return fmt.Errorf("processing create shard event: %s", err)
				}
			case shardstates.EVENT_SHARD_CONFIG_UPDATE:
				cfgEvt := &shardstates.ConfigShardEvent{}
				if err := cfgEvt.Deserialize(bytes.NewBuffer(shardEvt.Payload)); err != nil {
					return fmt.Errorf("deserialize create shard event: %s", err)
				}
				if err := self.onShardConfigured(cfgEvt); err != nil {
					return fmt.Errorf("processing create shard event: %s", err)
				}
			case shardstates.EVENT_SHARD_PEER_JOIN:
				jointEvt := &shardstates.PeerJoinShardEvent{}
				if err := jointEvt.Deserialize(bytes.NewBuffer(shardEvt.Payload)); err != nil {
					return fmt.Errorf("deserialize join shard event: %s", err)
				}
				if err := self.onShardPeerJoint(jointEvt); err != nil {
					return fmt.Errorf("processing join shard event: %s", err)
				}
			case shardstates.EVENT_SHARD_ACTIVATED:
				evt := &shardstates.ShardActiveEvent{}
				if err := evt.Deserialize(bytes.NewBuffer(shardEvt.Payload)); err != nil {
					return fmt.Errorf("deserialize join shard event: %s", err)
				}
				if err := self.onShardActivated(evt); err != nil {
					return fmt.Errorf("processing join shard event: %s", err)
				}
			case shardstates.EVENT_SHARD_PEER_LEAVE:
			case shardstates.EVENT_SHARD_GAS_DEPOSIT: fallthrough
			case shardstates.EVENT_SHARD_GAS_WITHDRAW_REQ: fallthrough
			case shardstates.EVENT_SHARD_GAS_WITHDRAW_DONE:
				if err := self.onLocalShardEvent(shardEvt); err != nil {
					return fmt.Errorf("processing shard %d gas deposit: %s", shardEvt.ToShard, err)
				}
			}
			break
		case blk := <-self.localBlockMsgC:
			if err := self.onBlockPersistCompleted(blk); err != nil {
				return fmt.Errorf("processing shard %d, block %d, err: %s", self.shardID, blk.Header.Height, err)
			}
		case <-self.quitC:
			return nil
		}
	}
	// get genesis block
	// init ledger if needed
	// verify genesis block if needed

	//

	return nil
}

func (self *ChainManager) broadcastMsgLoop() {
	self.quitWg.Add(1)
	defer self.quitWg.Done()

	for {
		select {
		case msg := <-self.broadcastMsgC:
			if msg.targetShardID == math.MaxUint64 {
				// broadcast
				for _, s := range self.shards {
					if s.Connected && s.Sender != nil {
						s.Sender.Tell(msg.msg)
					}
				}
			} else {
				// send to shard
				if s, present := self.shards[msg.targetShardID]; present {
					if s.Connected && s.Sender != nil {
						s.Sender.Tell(msg.msg)
					}
				} else {
					// other shards
					// TODO: send msg to sib shards
				}
			}
		case <-self.quitC:
			return
		}
	}
}

func (self *ChainManager) remoteShardMsgLoop() {
	self.quitWg.Add(1)
	defer self.quitWg.Done()

	for {
		if err := self.processRemoteShardMsg(); err != nil {
			log.Errorf("chain mgr process remote shard msg failed: %s", err)
		}
		select {
		case <-self.quitC:
			return
		default:
		}
	}
}

func (self *ChainManager) processRemoteShardMsg() error {
	select {
	case remoteMsg := <-self.remoteShardMsgC:
		msg := remoteMsg.Msg
		switch msg.Type() {
		case shardmsg.HELLO_MSG:
			helloMsg, ok := msg.(*shardmsg.ShardHelloMsg)
			if !ok {
				return fmt.Errorf("invalid hello msg")
			}
			if helloMsg.TargetShardID != self.shardID {
				return fmt.Errorf("hello msg with invalid target %d, from %d", helloMsg.TargetShardID, helloMsg.SourceShardID)
			}
			log.Infof(">>>>>> received hello msg from %d", helloMsg.SourceShardID)
			// response ack
			return self.onNewShardConnected(remoteMsg.Sender, helloMsg)
		case shardmsg.CONFIG_MSG:
			shardCfgMsg, ok := msg.(*shardmsg.ShardConfigMsg)
			if !ok {
				return fmt.Errorf("invalid config msg")
			}
			log.Infof(">>>>>> shard %d received config msg", self.shardID)
			return self.onShardConfigRequest(remoteMsg.Sender, shardCfgMsg)
		case shardmsg.BLOCK_REQ_MSG:
		case shardmsg.BLOCK_RSP_MSG:
			blkMsg, ok := msg.(*shardmsg.ShardBlockRspMsg)
			if !ok {
				return fmt.Errorf("invalid block rsp msg")
			}
			return self.onShardBlockReceived(remoteMsg.Sender, blkMsg)
		case shardmsg.PEERINFO_REQ_MSG:
		case shardmsg.PEERINFO_RSP_MSG:
			return nil
		case shardmsg.DISCONNECTED_MSG:
			disconnMsg, ok := msg.(*shardmsg.ShardDisconnectedMsg)
			if !ok {
				return fmt.Errorf("invalid disconnect message")
			}
			return self.onShardDisconnected(disconnMsg)
		default:
			return nil
		}
	case <-self.quitC:
		return nil
	}
	return nil
}

func (self *ChainManager) connectParent() error {

	// connect to parent
	if self.parentShardID == math.MaxUint64 {
		return nil
	}
	if self.localPid == nil {
		return fmt.Errorf("shard %d connect parent with nil localPid", self.shardID)
	}

	parentAddr := fmt.Sprintf("%s:%d", self.parentShardIPAddress, self.parentShardPort)
	parentPid := actor.NewPID(parentAddr, fmt.Sprintf("shard-%d", self.parentShardID))
	hellomsg, err := shardmsg.NewShardHelloMsg(self.shardID, self.parentShardID, self.localPid)
	if err != nil {
		return fmt.Errorf("build hello msg: %s", err)
	}
	parentPid.Request(hellomsg, self.localPid)
	if err := self.waitConnectParent(CONNECT_PARENT_TIMEOUT); err != nil {
		return fmt.Errorf("wait connection with parent err: %s", err)
	}

	self.parentPid = parentPid
	log.Infof("shard %d connected with parent shard %d", self.shardID, self.parentShardID)
	return nil
}

func (self *ChainManager) waitConnectParent(timeout time.Duration) error {
	select {
	case <-time.After(timeout):
		return fmt.Errorf("wait parent connection timeout")
	case connected := <-self.parentConnWait:
		if connected {
			return nil
		}
		return fmt.Errorf("connection failed")
	}
	return nil
}

func (self *ChainManager) notifyParentConnected() {
	self.parentConnWait <- true
}

func (self *ChainManager) startListener() error {

	// start local
	props := actor.FromProducer(func() actor.Actor {
		return self
	})
	pid, err := actor.SpawnNamed(props, fmt.Sprintf("shard-%d", self.shardID))
	if err != nil {
		return fmt.Errorf("init chain manager actor: %s", err)
	}
	self.localPid = pid

	log.Infof("chain %d started listen on port %d", self.shardID, self.shardPort)
	return nil
}

func (self *ChainManager) Stop() {
	close(self.quitC)
	self.quitWg.Wait()
}

func (self *ChainManager) broadcastShardMsg(msg *shardmsg.CrossShardMsg) {
	self.broadcastMsgC <- &MsgSendReq{
		targetShardID: math.MaxUint64,
		msg:           msg,
	}
}

func (self *ChainManager) sendShardMsg(shardId uint64, msg *shardmsg.CrossShardMsg) {
	self.broadcastMsgC <- &MsgSendReq{
		targetShardID: shardId,
		msg:           msg,
	}
}