package dendrite

import (
	"github.com/golang/protobuf/proto"
	zmq "github.com/pebbe/zmq4"
	"log"
	//"sync"
	"bytes"
	"fmt"
	"time"
)

const (
	// protocol buffer messages (for definitions, see pb_defs/chord.go)
	pbPing MsgType = iota
	pbAck
	pbErr
	pbForward
	pbJoin
	pbLeave
	pbListVnodes
	pbListVnodesResp
	pbFindSuccessors
)

func (transport *ZMQTransport) newErrorMsg(msg string) *ChordMsg {
	pbmsg := &PBProtoErr{
		Error: proto.String(msg),
	}
	pbdata, _ := proto.Marshal(pbmsg)
	return &ChordMsg{
		Type: pbErr,
		Data: pbdata,
	}

}
func (transport *ZMQTransport) Encode(mt MsgType, data []byte) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(byte(mt))
	buf.Write(data)
	return buf.Bytes()
}

func (transport *ZMQTransport) Decode(data []byte) (*ChordMsg, error) {
	data_len := len(data)
	if data_len == 0 {
		return nil, fmt.Errorf("data too short: %d", len(data))
	}

	cm := &ChordMsg{Type: MsgType(data[0])}

	if data_len > 1 {
		cm.Data = data[1:]
	}

	// parse the data and set the handler
	switch cm.Type {
	case pbPing:
		var pingMsg PBProtoPing
		err := proto.Unmarshal(cm.Data, &pingMsg)
		if err != nil {
			return nil, fmt.Errorf("error decoding PBProtoPing message - %s", err)
		}
		cm.TransportMsg = pingMsg
		cm.TransportHandler = transport.zmq_ping_handler
	case pbErr:
		var errorMsg PBProtoErr
		err := proto.Unmarshal(cm.Data, &errorMsg)
		if err != nil {
			return nil, fmt.Errorf("error decoding PBProtoErr message - %s", err)
		}
		cm.TransportMsg = errorMsg
		cm.TransportHandler = transport.zmq_error_handler
	case pbForward:
		var forwardMsg PBProtoForward
		err := proto.Unmarshal(cm.Data, &forwardMsg)
		if err != nil {
			return nil, fmt.Errorf("error decoding PBProtoForward message - %s", err)
		}
		cm.TransportMsg = forwardMsg
	case pbLeave:
		var leaveMsg PBProtoLeave
		err := proto.Unmarshal(cm.Data, &leaveMsg)
		if err != nil {
			return nil, fmt.Errorf("error decoding PBProtoLeave message - %s", err)
		}
		cm.TransportMsg = leaveMsg
		cm.TransportHandler = transport.zmq_leave_handler
	case pbListVnodes:
		var listVnodesMsg PBProtoListVnodes
		err := proto.Unmarshal(cm.Data, &listVnodesMsg)
		if err != nil {
			return nil, fmt.Errorf("error decoding PBProtoListVnodes message - %s", err)
		}
		cm.TransportMsg = listVnodesMsg
		cm.TransportHandler = transport.zmq_listVnodes_handler
	case pbListVnodesResp:
		var listVnodesRespMsg PBProtoListVnodesResp
		err := proto.Unmarshal(cm.Data, &listVnodesRespMsg)
		if err != nil {
			return nil, fmt.Errorf("error decoding PBProtoListVnodesResp message - %s", err)
		}
		cm.TransportMsg = listVnodesRespMsg
	case pbFindSuccessors:
		var findSuccMsg PBProtoFindSuccessors
		err := proto.Unmarshal(cm.Data, &findSuccMsg)
		if err != nil {
			return nil, fmt.Errorf("error decoding PBProtoFindSuccessors message - %s", err)
		}
		cm.TransportMsg = findSuccMsg
		cm.TransportHandler = transport.zmq_find_successors_handler
	default:
		return nil, fmt.Errorf("error decoding message - unknown request type %x", cm.Type)
	}

	return cm, nil
}

// Client Request: list of vnodes from remote host
func (transport *ZMQTransport) ListVnodes(host string) ([]*Vnode, error) {
	req_sock, err := transport.zmq_context.NewSocket(zmq.REQ)
	if err != nil {
		return nil, err
	}
	defer req_sock.Close()

	err = req_sock.Connect("tcp://" + host)
	if err != nil {
		return nil, err
	}
	error_c := make(chan error, 1)
	resp_c := make(chan []*Vnode, 1)

	go func() {
		// Build request protobuf
		req := new(PBProtoListVnodes)
		reqData, _ := proto.Marshal(req)
		encoded := transport.Encode(pbListVnodes, reqData)
		req_sock.SendBytes(encoded, 0)

		// read response and decode it
		resp, err := req_sock.RecvBytes(0)
		if err != nil {
			error_c <- fmt.Errorf("transport::ListVnodes - error while reading response - %s", err)
			return
		}
		decoded, err := transport.Decode(resp)
		if err != nil {
			error_c <- fmt.Errorf("transport::ListVnodes - error while decoding response - %s", err)
			return
		}

		switch decoded.Type {
		case pbErr:
			pbMsg := decoded.TransportMsg.(PBProtoErr)
			error_c <- fmt.Errorf("transport::ListVnodes - got error response - %s", pbMsg.GetError())
		case pbListVnodesResp:
			pbMsg := decoded.TransportMsg.(PBProtoListVnodesResp)
			vnodes := make([]*Vnode, len(pbMsg.GetVnodes()))
			for idx, pbVnode := range pbMsg.GetVnodes() {
				vnodes[idx] = &Vnode{Id: pbVnode.GetId(), Host: pbVnode.GetHost()}
			}
			resp_c <- vnodes
			return
		default:
			// unexpected response
			error_c <- fmt.Errorf("transport::ListVnodes - unexpected response")
			return
		}
	}()

	select {
	case <-time.After(transport.clientTimeout):
		return nil, fmt.Errorf("transport::ListVnodes - command timed out!")
	case err := <-error_c:
		return nil, err
	case resp_vnodes := <-resp_c:
		return resp_vnodes, nil
	}

}

// Client Request: find successors for vnode key, by asking remote vnode
func (transport *ZMQTransport) FindSuccessors(remote *Vnode, limit int, key []byte) ([]*Vnode, error) {
	req_sock, err := transport.zmq_context.NewSocket(zmq.REQ)
	if err != nil {
		return nil, err
	}
	defer req_sock.Close()
	err = req_sock.Connect("tcp://" + remote.Host)
	if err != nil {
		return nil, err
	}
	error_c := make(chan error, 1)
	resp_c := make(chan []*Vnode, 1)
	forward_c := make(chan *Vnode, 1)

	go func() {
		// Build request protobuf
		dest := &PBProtoVnode{
			Host: proto.String(remote.Host),
			Id:   remote.Id,
		}
		req := &PBProtoFindSuccessors{
			Dest:  dest,
			Key:   key,
			Limit: proto.Int32(int32(limit)),
		}
		reqData, _ := proto.Marshal(req)
		encoded := transport.Encode(pbFindSuccessors, reqData)
		req_sock.SendBytes(encoded, 0)

		// read response and decode it
		resp, err := req_sock.RecvBytes(0)
		if err != nil {
			error_c <- fmt.Errorf("transport::FindSuccessors - error while reading response - %s", err)
			return
		}
		decoded, err := transport.Decode(resp)
		if err != nil {
			error_c <- fmt.Errorf("transport::FindSuccessors - error while decoding response - %s", err)
			return
		}

		switch decoded.Type {
		case pbErr:
			pbMsg := decoded.TransportMsg.(PBProtoErr)
			error_c <- fmt.Errorf("transport::FindSuccessors - got error response - %s", pbMsg.GetError())
			return
		case pbForward:
			pbMsg := decoded.TransportMsg.(PBProtoForward)
			vnode := &Vnode{
				Id:   pbMsg.GetVnode().GetId(),
				Host: pbMsg.GetVnode().GetHost(),
			}
			forward_c <- vnode
			return
		case pbListVnodesResp:
			pbMsg := decoded.TransportMsg.(PBProtoListVnodesResp)
			vnodes := make([]*Vnode, len(pbMsg.GetVnodes()))
			for idx, pbVnode := range pbMsg.GetVnodes() {
				vnodes[idx] = &Vnode{Id: pbVnode.GetId(), Host: pbVnode.GetHost()}
			}
			resp_c <- vnodes
			return
		default:
			// unexpected response
			error_c <- fmt.Errorf("transport::FindSuccessors - unexpected response")
			return
		}
	}()

	select {
	case <-time.After(transport.clientTimeout):
		return nil, fmt.Errorf("transport::FindSuccessors - command timed out!")

	case err := <-error_c:
		return nil, err
	case new_remote := <-forward_c:
		return transport.FindSuccessors(new_remote, limit, key)
	case resp_vnodes := <-resp_c:
		return resp_vnodes, nil
	}

}
func (transport *ZMQTransport) Ping(remote_vn *Vnode) (bool, error) {
	req_sock, err := transport.zmq_context.NewSocket(zmq.REQ)
	if err != nil {
		return false, err
	}
	defer req_sock.Close()

	err = req_sock.Connect("tcp://" + remote_vn.Host)
	if err != nil {
		return false, err
	}
	pbPingMsg := &PBProtoPing{
		Version: proto.Int64(1),
	}
	pbPingData, _ := proto.Marshal(pbPingMsg)
	encoded := transport.Encode(pbPing, pbPingData)
	req_sock.SendBytes(encoded, 0)
	resp, _ := req_sock.RecvBytes(0)
	decoded, err := transport.Decode(resp)
	if err != nil {
		return false, err
	}
	pongMsg := new(PBProtoPing)
	err = proto.Unmarshal(decoded.Data, pongMsg)
	if err != nil {
		return false, err
	}
	fmt.Println("Got pong with version:", pongMsg.GetVersion())
	return true, nil
}
