package dendrite

import (
	"bytes"
	"fmt"
	"log"
	"sort"
	"time"
)

type MsgType byte
type ChordMsg struct {
	Type             MsgType
	Data             []byte
	TransportMsg     interface{}                     // unmarshalled data, depending on transport
	TransportHandler func(*ChordMsg, chan *ChordMsg) // request pointer, response channel
}

type ErrHookUnknownType string

func (e ErrHookUnknownType) Error() string {
	return fmt.Sprintf("%s", string(e))
}

// TransportHook is used to extend base capabilities of a transport in decoding and processing messages
// 3rd party packages can register their hooks and leverage existing transport architecture and capabilities
// ZMQTransport allows this extension
type TransportHook interface {
	// decodes bytes to ChordMsg
	Decode([]byte) (*ChordMsg, error)
}

// DelegateHook is used to extend capabilities on events when ring structure changes
// specifically, when new predecessor is set.
// first Vnode param is local Vnode that handles delegation
// second Vnode param is a Vnode representing new predecessor
type DelegateHook interface {
	EmitEvent(*EventCtx)
}

type Transport interface {
	// Gets a list of the vnodes on the box
	ListVnodes(string) ([]*Vnode, error)

	// Ping a Vnode, check for liveness
	Ping(*Vnode) (bool, error)

	// Request a vnode's predecessor
	GetPredecessor(*Vnode) (*Vnode, error)

	// Notify our successor of ourselves
	Notify(dest, self *Vnode) ([]*Vnode, error)

	// Find successors for vnode key
	FindSuccessors(*Vnode, int, []byte) ([]*Vnode, error)

	// Find vnodeHandler if it exists locally
	GetVnodeHandler(*Vnode) (VnodeHandler, bool)

	// Clears a predecessor if it matches a given vnode. Used to leave.
	//ClearPredecessor(target, self *Vnode) error

	// Instructs a node to skip a given successor. Used to leave.
	//SkipSuccessor(target, self *Vnode) error

	// Register vnode handlers
	Register(*Vnode, VnodeHandler)

	// encode encodes dendrite msg into two frame byte stream
	// first byte is message type, and the rest is protobuf data
	Encode(MsgType, []byte) []byte
	RegisterHook(TransportHook)
	TransportHook
}

type Config struct {
	Hostname      string
	NumVnodes     int // num of vnodes to create
	StabilizeMin  time.Duration
	StabilizeMax  time.Duration
	NumSuccessors int      // number of successor to keep in self log
	Replicas      int      // number of replicas to keep by default
	LogLevel      LogLevel // logLevel, 0 = info, 1 = debug
}

func DefaultConfig(hostname string) *Config {
	return &Config{
		Hostname: hostname,
		// NumVnodes should be set around logN
		// N is approximate number of real nodes in cluster
		// this way we get O(logN) lookup speed
		NumVnodes:     3,
		StabilizeMin:  1 * time.Second,
		StabilizeMax:  3 * time.Second,
		NumSuccessors: 8, // number of known successors to keep track with
		Replicas:      2,
		LogLevel:      LogInfo,
	}
}

type LogLevel int

const (
	LogNull  LogLevel = 0
	LogInfo  LogLevel = 1
	LogDebug LogLevel = 2
)

func (r *Ring) Logf(level LogLevel, format string, v ...interface{}) {
	var new_format string
	if level == LogInfo {
		new_format = "[DENDRITE][INFO] " + format
	} else if level == LogDebug {
		new_format = "[DENDRITE][DEBUG] " + format
	}

	if r.config.LogLevel == LogDebug {
		log.Printf(new_format, v...)
	} else if r.config.LogLevel == LogInfo && level == LogInfo {
		log.Printf(new_format, v...)
	}
}

func (r *Ring) Logln(level LogLevel, v ...interface{}) {
	var new_format string
	if level == LogInfo {
		new_format = "[DENDRITE][INFO]"
	} else if level == LogDebug {
		new_format = "[DENDRITE][DEBUG]"
	}
	if r.config.LogLevel == LogDebug {
		v = append([]interface{}{new_format}, v...)
		log.Println(v...)
	} else if r.config.LogLevel == LogInfo && level == LogInfo {
		v = append([]interface{}{new_format}, v...)
		log.Println(v...)
	}
}

type Ring struct {
	config         *Config
	transport      Transport
	vnodes         []*localVnode
	shutdown       chan bool
	Stabilizations int
	delegateHooks  []DelegateHook
}

// implement sort.Interface (Len(), Less() and Swap())
func (r *Ring) Less(i, j int) bool {
	return bytes.Compare(r.vnodes[i].Id, r.vnodes[j].Id) == -1
}

func (r *Ring) Swap(i, j int) {
	r.vnodes[i], r.vnodes[j] = r.vnodes[j], r.vnodes[i]
}

func (r *Ring) Len() int {
	return len(r.vnodes)
}

func (r *Ring) Replicas() int {
	return r.config.Replicas
}

func (r *Ring) MaxStabilize() time.Duration {
	return r.config.StabilizeMax
}

// Does a key lookup for up to N successors of a key
func (r *Ring) Lookup(n int, keyHash []byte) ([]*Vnode, error) {
	// Ensure that n is sane
	if n > r.config.NumSuccessors {
		return nil, fmt.Errorf("Cannot ask for more successors than NumSuccessors!")
	}

	// Find the nearest local vnode
	nearest := nearestVnodeToKey(r.vnodes, keyHash)

	// Use the nearest node for the lookup
	successors, err := r.transport.FindSuccessors(nearest, n, keyHash)
	if err != nil {
		return nil, err
	}

	// Trim the nil successors
	for successors[len(successors)-1] == nil {
		successors = successors[:len(successors)-1]
	}
	return successors, nil
}

// Initializes the vnodes with their local successors
// Vnodes need to be sorted before this method is called
func (r *Ring) setLocalSuccessors() {
	numV := len(r.vnodes)
	if numV == 1 {
		for _, vnode := range r.vnodes {
			vnode.successors[0] = &vnode.Vnode
		}
		return
	}
	// we use numV-1 in order to avoid setting ourselves as last successor
	numSuc := min(r.config.NumSuccessors, numV-1)
	for idx, vnode := range r.vnodes {
		for i := 0; i < numSuc; i++ {
			vnode.successors[i] = &r.vnodes[(idx+i+1)%numV].Vnode
		}
	}

}

func (r *Ring) init(config *Config, transport Transport) {
	r.config = config
	r.transport = InitLocalTransport(transport)
	r.vnodes = make([]*localVnode, config.NumVnodes)
	r.shutdown = make(chan bool)
	r.delegateHooks = make([]DelegateHook, 0)
	// initialize vnodes
	for i := 0; i < config.NumVnodes; i++ {
		vn := &localVnode{}
		r.vnodes[i] = vn
		vn.ring = r
		vn.init(i)
	}
	sort.Sort(r)

	// for i := 0; i < config.NumVnodes; i++ {
	// 	vn := r.vnodes[i]
	// 	if i == 0 {
	// 		vn.predecessor = &r.vnodes[config.NumVnodes-1].Vnode
	// 	} else {
	// 		vn.predecessor = &r.vnodes[i-1].Vnode
	// 	}
	// }
	/*
		go func() {
			for {
				for _, vnode := range r.vnodes {
					var pred []byte
					if vnode.predecessor == nil {
						pred = nil
					} else {
						pred = vnode.predecessor.Id
					}

					fmt.Printf("Vnode: %X -> pred: %X -> succ: ", vnode.Id, pred)
					for idx, suc := range vnode.successors {
						if suc == nil {
							break
						}
						fmt.Printf("succ-%d-%X, ", idx, suc.Id)
					}
					fmt.Printf("\n")
				}
				time.Sleep(15 * time.Second)
			}
		}()
	*/
}

func (r *Ring) schedule() {
	for i := 0; i < len(r.vnodes); i++ {
		r.vnodes[i].schedule()
	}
}

func (r *Ring) MyVnodes() []*Vnode {
	rv := make([]*Vnode, len(r.vnodes))
	for idx, local_vn := range r.vnodes {
		rv[idx] = &local_vn.Vnode
	}
	return rv
}

func CreateRing(config *Config, transport Transport) (*Ring, error) {
	// initialize the ring and sort vnodes
	r := &Ring{}
	r.init(config, transport)

	// for each vnode, setup local successors
	r.setLocalSuccessors()

	// schedule vnode stabilizers
	r.schedule()

	return r, nil
}

func JoinRing(config *Config, transport Transport, existing string) (*Ring, error) {
	hosts, err := transport.ListVnodes(existing)
	if err != nil {
		return nil, err
	}
	if hosts == nil || len(hosts) == 0 {
		return nil, fmt.Errorf("Remote host has no vnodes registered yet")
	}

	// initialize the ring and sort vnodes
	r := &Ring{}
	r.init(config, transport)

	// for each vnode, get the new list of live successors from remote
	for _, vn := range r.vnodes {
		resolved := false
		var last_error error
		// go through each host until we get successor list from one of them
	L:
		for _, remote_host := range hosts {
			suc_pos := 0
			succs, err := transport.FindSuccessors(remote_host, config.NumSuccessors, vn.Id)
			if err != nil {
				last_error = err
				continue L
			}
			if succs == nil || len(succs) == 0 {
				//return nil, fmt.Errorf("Failed to find successors for vnode, got empty list")
				last_error = fmt.Errorf("Failed to find successors for vnode, got empty list")
				continue L
			}
		SL:
			for _, s := range succs {
				// if we're rejoining before failure is detected.. s could be us
				if bytes.Compare(vn.Id, s.Id) == 0 {
					continue SL
				}
				if s == nil {
					break SL
				}
				vn.successors[suc_pos] = s
				suc_pos += 1
			}
			resolved = true
			break L
		}
		if !resolved {
			return nil, fmt.Errorf("Exhausted all remote vnodes while trying to get the list of successors. Last error: %s", last_error.Error())
		}

	}
	r.transport.Ping(&Vnode{Host: existing})

	// We can now initiate stabilization protocol
	for _, vn := range r.vnodes {
		vn.stabilize()
	}
	return r, nil
}

func (r *Ring) RegisterDelegateHook(dh DelegateHook) {
	r.delegateHooks = append(r.delegateHooks, dh)
}

type RingEventType int

var (
	EvPredecessorJoined RingEventType = 1
	EvPredecessorLeft   RingEventType = 2
	EvReplicasChanged   RingEventType = 3
)

type EventCtx struct {
	EvType        RingEventType
	Target        *Vnode
	PrimaryItem   *Vnode
	SecondaryItem *Vnode
	ItemList      []*Vnode
	ResponseCh    chan interface{}
}

func (r *Ring) emit(ctx *EventCtx) {
	for _, dh := range r.delegateHooks {
		go dh.EmitEvent(ctx)
	}
}
