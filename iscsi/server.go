package iscsi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxSegment  = 8192
	maxTransfer = 8 << 20
)

var networkOrder = binary.BigEndian

type Backend interface {
	io.ReaderAt
	io.WriterAt
	Size() int64
	ReadOnly() bool
	Sync() error
}

type Options struct {
	Listen     string
	TargetName string
}

type Info struct {
	Portal     string `json:"portal"`
	TargetName string `json:"targetName"`
	LUN        int    `json:"lun"`
	Size       int64  `json:"size"`
	ReadOnly   bool   `json:"readOnly"`
}

type target struct {
	backend     Backend
	name        string
	mu          sync.Mutex
	active      bool
	connections map[net.Conn]struct{}
	workers     sync.WaitGroup
	syncErr     error
}

func Serve(ctx context.Context, backend Backend, options Options, ready func(Info)) error {
	if backend.Size() <= 0 || backend.Size()%512 != 0 {
		return errors.New("iscsi: size must be a positive multiple of 512")
	}
	if !strings.HasPrefix(options.TargetName, "iqn.") || len(options.TargetName) > 223 || strings.ContainsAny(options.TargetName, "\x00=, \t\r\n") {
		return errors.New("iscsi: target name must be an IQN of at most 223 bytes")
	}
	address, err := net.ResolveTCPAddr("tcp", options.Listen)
	if err != nil {
		return err
	}
	if !address.IP.IsLoopback() {
		return errors.New("iscsi: only loopback listeners are supported (authentication is disabled)")
	}
	listener, err := net.ListenTCP("tcp", address)
	if err != nil {
		return err
	}
	server := &target{backend: backend, name: options.TargetName, connections: make(map[net.Conn]struct{})}
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	defer listener.Close()
	if ready != nil {
		ready(Info{Portal: listener.Addr().String(), TargetName: server.name, Size: backend.Size(), ReadOnly: backend.ReadOnly()})
	}
	for {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() == nil {
				err = acceptErr
			}
			break
		}
		server.mu.Lock()
		if len(server.connections) >= 16 {
			server.mu.Unlock()
			connection.Close()
			continue
		}
		server.connections[connection] = struct{}{}
		server.workers.Add(1)
		server.mu.Unlock()
		go server.handle(connection)
	}
	server.mu.Lock()
	for connection := range server.connections {
		connection.Close()
	}
	server.mu.Unlock()
	server.workers.Wait()
	return errors.Join(err, server.syncErr, backend.Sync())
}

type packet struct {
	header [48]byte
	data   []byte
}

func readPacket(reader io.Reader) (packet, error) {
	var message packet
	if _, err := io.ReadFull(reader, message.header[:]); err != nil {
		return message, err
	}
	length := int(message.header[5])<<16 | int(message.header[6])<<8 | int(message.header[7])
	if message.header[4] != 0 || length > maxSegment {
		return message, errors.New("iscsi: unsupported AHS or oversized data segment")
	}
	buffer := make([]byte, (length+3)&^3)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return message, err
	}
	message.data = buffer[:length]
	return message, nil
}

func writePacket(writer io.Writer, message packet) error {
	length := len(message.data)
	message.header[5], message.header[6], message.header[7] = byte(length>>16), byte(length>>8), byte(length)
	buffer := make([]byte, 48+(length+3)&^3)
	copy(buffer, message.header[:])
	copy(buffer[48:], message.data)
	for len(buffer) > 0 {
		count, err := writer.Write(buffer)
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
		buffer = buffer[count:]
	}
	return nil
}

type session struct {
	target      *target
	connection  net.Conn
	statSN      uint32
	cmdSN       uint32
	peerSegment int
	maxBurst    int
	firstBurst  int
	immediate   bool
	discovery   bool
	claimed     bool
	busy        bool
	sense       []byte
}

func (server *target) handle(connection net.Conn) {
	client := &session{target: server, connection: connection, peerSegment: 8192, maxBurst: 262144, firstBurst: 65536, immediate: true}
	defer func() {
		connection.Close()
		var syncErr error
		if client.claimed {
			syncErr = server.backend.Sync()
		}
		server.mu.Lock()
		if server.syncErr == nil {
			server.syncErr = syncErr
		}
		if client.claimed {
			server.active = false
		}
		delete(server.connections, connection)
		server.mu.Unlock()
		server.workers.Done()
	}()
	connection.SetDeadline(time.Now().Add(30 * time.Second))
	if err := client.login(); err != nil {
		return
	}
	connection.SetDeadline(time.Time{})
	for {
		message, err := readPacket(connection)
		if err != nil {
			return
		}
		if message.header[0]&0x40 == 0 {
			if networkOrder.Uint32(message.header[24:]) != client.cmdSN {
				return
			}
			client.cmdSN++
		}
		switch message.header[0] & 0x3f {
		case 0x00:
			err = client.nop(message)
		case 0x01:
			if client.discovery {
				return
			}
			client.busy = true
			err = client.command(message)
		case 0x02:
			response := client.response(0x22, message)
			response.header[2] = 5
			if message.header[1]&0x7f == 1 {
				response.header[2] = 1
			}
			err = client.send(response, true)
		case 0x04:
			err = client.text(message)
		case 0x06:
			response := client.response(0x26, message)
			if message.header[1]&0x7f > 1 {
				response.header[2] = 2
			}
			client.send(response, true)
			return
		default:
			return
		}
		if err != nil {
			return
		}
	}
}

func (client *session) response(opcode byte, request packet) packet {
	var response packet
	response.header[0], response.header[1] = opcode, 0x80
	copy(response.header[16:20], request.header[16:20])
	return response
}

func (client *session) send(response packet, advance bool) error {
	if response.header[0] == 0x21 {
		client.busy = false
	}
	maximum := client.cmdSN
	if client.busy {
		maximum--
	}
	networkOrder.PutUint32(response.header[24:], client.statSN)
	networkOrder.PutUint32(response.header[28:], client.cmdSN)
	networkOrder.PutUint32(response.header[32:], maximum)
	if advance {
		client.statSN++
	}
	client.connection.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return writePacket(client.connection, response)
}

func textPairs(data []byte) (map[string]string, error) {
	values := make(map[string]string)
	if len(data) > 0 && data[len(data)-1] != 0 {
		return nil, errors.New("iscsi: unterminated text")
	}
	for _, pair := range strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00") {
		if pair == "" {
			continue
		}
		key, value, ok := strings.Cut(pair, "=")
		if _, duplicate := values[key]; !ok || duplicate || key == "" {
			return nil, errors.New("iscsi: invalid text key")
		}
		values[key] = value
	}
	return values, nil
}

func hasChoice(value, choice string) bool {
	for _, candidate := range strings.Split(value, ",") {
		if candidate == choice {
			return true
		}
	}
	return false
}

func (client *session) negotiate(key, value string) (string, bool, error) {
	switch key {
	case "InitiatorName", "InitiatorAlias", "TargetName", "SessionType":
		return "", false, nil
	case "AuthMethod", "HeaderDigest", "DataDigest":
		if !hasChoice(value, "None") {
			return "", false, errors.New("iscsi: only AuthMethod=None and digests=None are supported")
		}
		return "None", true, nil
	case "InitialR2T", "DataPDUInOrder", "DataSequenceInOrder":
		if value != "Yes" && value != "No" {
			return "", false, errors.New("iscsi: invalid boolean")
		}
		return "Yes", true, nil
	case "ImmediateData":
		if value != "Yes" && value != "No" {
			return "", false, errors.New("iscsi: invalid boolean")
		}
		client.immediate = value == "Yes"
		return value, true, nil
	case "MaxRecvDataSegmentLength", "MaxBurstLength", "FirstBurstLength", "MaxConnections", "MaxOutstandingR2T", "ErrorRecoveryLevel", "DefaultTime2Wait", "DefaultTime2Retain":
		number, err := strconv.Atoi(value)
		if err != nil || number < 0 || number > 0xffffff {
			return "", false, errors.New("iscsi: invalid numeric parameter")
		}
		switch key {
		case "MaxRecvDataSegmentLength", "MaxBurstLength", "FirstBurstLength":
			if number < 512 {
				return "", false, errors.New("iscsi: invalid burst/segment size")
			}
			switch key {
			case "MaxRecvDataSegmentLength":
				client.peerSegment = min(number, maxSegment)
				return strconv.Itoa(maxSegment), true, nil
			case "MaxBurstLength":
				client.maxBurst = min(number, 262144)
				return strconv.Itoa(client.maxBurst), true, nil
			default:
				client.firstBurst = min(number, 65536)
				return strconv.Itoa(client.firstBurst), true, nil
			}
		case "MaxConnections", "MaxOutstandingR2T":
			if number < 1 || number > 65535 {
				return "", false, errors.New("iscsi: invalid connection/R2T count")
			}
			return "1", true, nil
		case "ErrorRecoveryLevel":
			if number > 2 {
				return "", false, errors.New("iscsi: invalid recovery level")
			}
			return "0", true, nil
		case "DefaultTime2Wait":
			if number > 3600 {
				return "", false, errors.New("iscsi: invalid recovery timeout")
			}
			return strconv.Itoa(max(number, 2)), true, nil
		default:
			if number > 3600 {
				return "", false, errors.New("iscsi: invalid recovery timeout")
			}
			return "0", true, nil
		}
	default:
		return "NotUnderstood", true, nil
	}
}

func (client *session) login() error {
	values := make(map[string]string)
	var identity [10]byte
	stage := byte(0)
	portalSent := false
	for round := 0; round < 16; round++ {
		request, err := readPacket(client.connection)
		if err != nil {
			return err
		}
		if request.header[0] != 0x43 || request.header[1]&0x40 != 0 {
			return errors.New("iscsi: invalid or continued login")
		}
		currentStage := (request.header[1] >> 2) & 3
		if round == 0 {
			copy(identity[:6], request.header[8:14])
			copy(identity[6:], request.header[16:20])
			client.cmdSN = networkOrder.Uint32(request.header[24:])
			stage = currentStage
		}
		response := client.response(0x23, request)
		response.header[1] = currentStage << 2
		copy(response.header[8:14], request.header[8:14])
		fail := func(detail byte) error {
			response.header[36], response.header[37] = 2, detail
			client.send(response, false)
			return errors.New("iscsi: login rejected")
		}
		if request.header[3] != 0 {
			return fail(5)
		}
		if networkOrder.Uint16(request.header[14:]) != 0 {
			return fail(0x0a)
		}
		if currentStage != stage || (stage != 0 && stage != 1) || string(identity[:6]) != string(request.header[8:14]) || string(identity[6:]) != string(request.header[16:20]) {
			return fail(0)
		}
		pairs, err := textPairs(request.data)
		if err != nil {
			return fail(0)
		}
		for key, value := range pairs {
			if prior, exists := values[key]; exists && prior != value {
				return fail(0)
			}
			values[key] = value
			answer, respond, negotiationErr := client.negotiate(key, value)
			if negotiationErr != nil {
				return fail(0)
			}
			if respond {
				response.data = append(response.data, []byte(key+"="+answer+"\x00")...)
			}
		}
		if request.header[1]&0x80 != 0 {
			nextStage := request.header[1] & 3
			if nextStage != 3 && !(stage == 0 && nextStage == 1) {
				return fail(0)
			}
			if values["InitiatorName"] == "" {
				return fail(7)
			}
			client.discovery = values["SessionType"] == "Discovery"
			if values["SessionType"] != "" && values["SessionType"] != "Normal" && !client.discovery {
				return fail(9)
			}
			if !client.discovery && values["TargetName"] != client.target.name {
				return fail(3)
			}
			if !portalSent && !client.discovery {
				response.data = append(response.data, []byte("TargetPortalGroupTag=1\x00")...)
				portalSent = true
			}
			if nextStage == 3 {
				if client.firstBurst > client.maxBurst {
					return fail(0)
				}
				if !client.discovery {
					client.target.mu.Lock()
					if !client.target.active {
						client.target.active, client.claimed = true, true
					}
					client.target.mu.Unlock()
					if !client.claimed {
						return fail(6)
					}
					networkOrder.PutUint16(response.header[14:], 1)
				}
			}
			response.header[1] |= 0x80 | nextStage
			stage = nextStage
		}
		if err := client.send(response, true); err != nil {
			return err
		}
		if stage == 3 {
			return nil
		}
	}
	return errors.New("iscsi: too many login rounds")
}

func (client *session) nop(request packet) error {
	if networkOrder.Uint32(request.header[16:]) == 0xffffffff {
		return nil
	}
	response := client.response(0x20, request)
	copy(response.header[8:16], request.header[8:16])
	networkOrder.PutUint32(response.header[20:], 0xffffffff)
	response.data = request.data
	if len(response.data) > client.peerSegment {
		return errors.New("iscsi: NOP echo exceeds negotiated segment length")
	}
	return client.send(response, true)
}

func (client *session) text(request packet) error {
	if request.header[1] != 0x80 || networkOrder.Uint32(request.header[20:]) != 0xffffffff {
		return errors.New("iscsi: continued text requests are unsupported")
	}
	pairs, err := textPairs(request.data)
	if err != nil {
		return err
	}
	response := client.response(0x24, request)
	networkOrder.PutUint32(response.header[20:], 0xffffffff)
	for key, value := range pairs {
		if key != "SendTargets" {
			response.data = append(response.data, []byte(key+"=NotUnderstood\x00")...)
		} else if value == "All" || value == client.target.name || value == "" {
			response.data = append(response.data, []byte("TargetName="+client.target.name+"\x00TargetAddress="+client.connection.LocalAddr().String()+",1\x00")...)
		}
	}
	if len(response.data) > client.peerSegment {
		return fmt.Errorf("iscsi: discovery response exceeds negotiated segment length")
	}
	return client.send(response, true)
}
