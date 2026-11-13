package iscsi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"
)

func senseData(key, code byte) []byte {
	data := make([]byte, 18)
	data[0], data[2], data[7], data[12] = 0x70, key, 10, code
	return data
}

func (client *session) check(request packet, key, code byte) error {
	client.sense = senseData(key, code)
	response := client.response(0x21, request)
	response.header[3] = 2
	response.data = append([]byte{0, byte(len(client.sense))}, client.sense...)
	return client.send(response, true)
}

func (client *session) result(request packet, data []byte) error {
	expected := networkOrder.Uint32(request.header[20:])
	actual := uint32(len(data))
	if actual > expected {
		data = data[:expected]
	}
	var sequence uint32
	for offset := 0; offset < len(data); sequence++ {
		length := min(len(data)-offset, client.peerSegment, client.maxBurst)
		response := client.response(0x25, request)
		networkOrder.PutUint32(response.header[20:], 0xffffffff)
		networkOrder.PutUint32(response.header[36:], sequence)
		networkOrder.PutUint32(response.header[40:], uint32(offset))
		response.data = data[offset : offset+length]
		if err := client.send(response, false); err != nil {
			return err
		}
		offset += length
	}
	response := client.response(0x21, request)
	networkOrder.PutUint32(response.header[36:], sequence)
	if request.header[1]&0x40 != 0 && actual != expected {
		if actual < expected {
			response.header[1] |= 2
			networkOrder.PutUint32(response.header[44:], expected-actual)
		} else {
			response.header[1] |= 4
			networkOrder.PutUint32(response.header[44:], actual-expected)
		}
	}
	return client.send(response, true)
}

func transferRange(command []byte) (uint64, uint64, bool, bool) {
	switch command[0] {
	case 0x08, 0x0a:
		blocks := uint64(command[4])
		if blocks == 0 {
			blocks = 256
		}
		return uint64(command[1]&0x1f)<<16 | uint64(command[2])<<8 | uint64(command[3]), blocks, command[0] == 0x0a, true
	case 0x28, 0x2a:
		return uint64(networkOrder.Uint32(command[2:])), uint64(networkOrder.Uint16(command[7:])), command[0] == 0x2a, true
	case 0xa8, 0xaa:
		return uint64(networkOrder.Uint32(command[2:])), uint64(networkOrder.Uint32(command[6:])), command[0] == 0xaa, true
	case 0x88, 0x8a:
		return networkOrder.Uint64(command[2:]), uint64(networkOrder.Uint32(command[10:])), command[0] == 0x8a, true
	}
	return 0, 0, false, false
}

func (client *session) command(request packet) error {
	command := request.header[32:48]
	backend := client.target.backend
	if networkOrder.Uint64(request.header[8:]) != 0 {
		return client.check(request, 5, 0x25)
	}
	if request.header[1]&0x80 == 0 || request.header[1]&0x60 == 0x60 {
		return client.check(request, 5, 0x24)
	}
	if block, count, write, ok := transferRange(command); ok {
		capacity := uint64(backend.Size() / 512)
		if block > capacity || count > capacity-block {
			return client.check(request, 5, 0x21)
		}
		if count > maxTransfer/512 || uint64(networkOrder.Uint32(request.header[20:])) != count*512 {
			return client.check(request, 5, 0x24)
		}
		if command[0] != 0x08 && command[0] != 0x0a && command[1]&0xe0 != 0 {
			return client.check(request, 5, 0x24)
		}
		length, offset := int(count*512), int64(block*512)
		if write {
			if backend.ReadOnly() {
				return client.check(request, 7, 0x27)
			}
			if request.header[1]&0x60 != 0x20 || len(request.data) > length || len(request.data) > client.firstBurst || (!client.immediate && len(request.data) != 0) {
				return client.check(request, 5, 0x24)
			}
			data, err := client.receiveWrite(request, length)
			if err != nil {
				return err
			}
			written, err := backend.WriteAt(data, offset)
			if err != nil || written != len(data) {
				return client.check(request, 3, 0x0c)
			}
			if command[0] != 0x0a && command[1]&8 != 0 {
				if err := backend.Sync(); err != nil {
					return client.check(request, 4, 0x44)
				}
			}
			return client.result(request, nil)
		}
		if request.header[1]&0x60 != 0x40 || len(request.data) != 0 {
			return client.check(request, 5, 0x24)
		}
		if command[0] != 0x08 && command[1]&8 != 0 {
			if err := backend.Sync(); err != nil {
				return client.check(request, 4, 0x44)
			}
		}
		data := make([]byte, length)
		read, err := backend.ReadAt(data, offset)
		if read != length || (err != nil && !errors.Is(err, io.EOF)) {
			return client.check(request, 3, 0x11)
		}
		return client.result(request, data)
	}
	if len(request.data) != 0 || request.header[1]&0x20 != 0 {
		return client.check(request, 5, 0x20)
	}
	var data []byte
	var allocation uint32
	switch command[0] {
	case 0x00, 0x1b, 0x1e:
		return client.result(request, nil)
	case 0x03:
		if command[1]&1 != 0 {
			return client.check(request, 5, 0x24)
		}
		data = client.sense
		if data == nil {
			data = senseData(0, 0)
		}
		client.sense = nil
		allocation = uint32(command[4])
	case 0x12:
		data = client.inquiry(command)
		if data == nil {
			return client.check(request, 5, 0x24)
		}
		allocation = uint32(networkOrder.Uint16(command[3:]))
	case 0x25:
		data = make([]byte, 8)
		networkOrder.PutUint32(data, uint32(min(uint64(backend.Size()/512-1), uint64(0xffffffff))))
		networkOrder.PutUint32(data[4:], 512)
		allocation = 8
	case 0x9e:
		if command[1] != 0x10 {
			return client.check(request, 5, 0x24)
		}
		data = make([]byte, 32)
		networkOrder.PutUint64(data, uint64(backend.Size()/512-1))
		networkOrder.PutUint32(data[8:], 512)
		allocation = networkOrder.Uint32(command[10:])
	case 0xa0:
		if command[2] > 2 {
			return client.check(request, 5, 0x24)
		}
		data = make([]byte, 16)
		networkOrder.PutUint32(data, 8)
		if command[2] == 1 {
			data = make([]byte, 8)
		}
		allocation = networkOrder.Uint32(command[6:])
	case 0x1a, 0x5a:
		data = client.modeSense(command)
		if data == nil {
			return client.check(request, 5, 0x24)
		}
		allocation = uint32(command[4])
		if command[0] == 0x5a {
			allocation = uint32(networkOrder.Uint16(command[7:]))
		}
	case 0x35, 0x91:
		if err := backend.Sync(); err != nil {
			return client.check(request, 4, 0x44)
		}
		return client.result(request, nil)
	default:
		return client.check(request, 5, 0x20)
	}
	if request.header[1]&0x40 == 0 && allocation != 0 {
		return client.check(request, 5, 0x24)
	}
	data = data[:min(uint32(len(data)), allocation)]
	return client.result(request, data)
}

func (client *session) receiveWrite(request packet, length int) ([]byte, error) {
	data := make([]byte, length)
	offset := copy(data, request.data)
	defer client.connection.SetReadDeadline(time.Time{})
	for sequence := uint32(0); offset < length; sequence++ {
		end := min(length, offset+client.maxBurst)
		transferTag := sequence + 1
		response := client.response(0x31, request)
		networkOrder.PutUint32(response.header[20:], transferTag)
		networkOrder.PutUint32(response.header[36:], sequence)
		networkOrder.PutUint32(response.header[40:], uint32(offset))
		networkOrder.PutUint32(response.header[44:], uint32(end-offset))
		if err := client.send(response, false); err != nil {
			return nil, err
		}
		for dataSequence := uint32(0); offset < end; {
			client.connection.SetReadDeadline(time.Now().Add(30 * time.Second))
			message, err := readPacket(client.connection)
			if err != nil {
				return nil, err
			}
			if message.header[0] == 0x40 {
				if err := client.nop(message); err != nil {
					return nil, err
				}
				continue
			}
			if message.header[0] != 0x05 || string(message.header[16:20]) != string(request.header[16:20]) || networkOrder.Uint32(message.header[20:]) != transferTag || networkOrder.Uint32(message.header[36:]) != dataSequence || networkOrder.Uint32(message.header[40:]) != uint32(offset) || len(message.data) > end-offset || len(message.data) == 0 {
				return nil, errors.New("iscsi: invalid Data-Out sequence")
			}
			offset += copy(data[offset:], message.data)
			if (message.header[1]&0x80 != 0) != (offset == end) {
				return nil, errors.New("iscsi: invalid Data-Out final flag")
			}
			dataSequence++
		}
	}
	return data, nil
}

func (client *session) inquiry(command []byte) []byte {
	if command[1]&0xfe != 0 {
		return nil
	}
	if command[1]&1 == 0 {
		if command[2] != 0 {
			return nil
		}
		data := make([]byte, 36)
		data[2], data[3], data[4] = 6, 2, 31
		copy(data[8:16], "OVERLAY ")
		copy(data[16:32], "OverlayBD Disk  ")
		copy(data[32:], "0001")
		return data
	}
	digest := sha256.Sum256([]byte(client.target.name))
	serial := hex.EncodeToString(digest[:8])
	var payload []byte
	switch command[2] {
	case 0:
		payload = []byte{0, 0x80, 0x83, 0xb0}
	case 0x80:
		payload = []byte(serial)
	case 0x83:
		identifier := "OVERLAY " + serial
		payload = append([]byte{2, 1, 0, byte(len(identifier))}, []byte(identifier)...)
	case 0xb0:
		payload = make([]byte, 60)
		networkOrder.PutUint32(payload[4:], maxTransfer/512)
	default:
		return nil
	}
	data := append([]byte{0, command[2], 0, byte(len(payload))}, payload...)
	return data
}

func (client *session) modeSense(command []byte) []byte {
	page, control := command[2]&0x3f, command[2]>>6
	if (page != 0x08 && page != 0x0a && page != 0x3f) || control == 3 || command[3] != 0 {
		return nil
	}
	headerSize := 4
	if command[0] == 0x5a {
		headerSize = 8
	}
	data := make([]byte, headerSize)
	deviceSpecific := 2
	if headerSize == 8 {
		deviceSpecific = 3
	}
	data[deviceSpecific] = 0x10
	if client.target.backend.ReadOnly() {
		data[deviceSpecific] |= 0x80
	}
	if command[1]&8 == 0 {
		descriptor := make([]byte, 8)
		descriptor[6] = 2
		data = append(data, descriptor...)
		data[headerSize-1] = 8
	}
	if page == 8 || page == 0x3f {
		cache := make([]byte, 20)
		cache[0], cache[1] = 8, 18
		if control != 1 {
			cache[2] = 4
		}
		data = append(data, cache...)
	}
	if page == 0x0a || page == 0x3f {
		controlPage := make([]byte, 12)
		controlPage[0], controlPage[1] = 0x0a, 10
		data = append(data, controlPage...)
	}
	if headerSize == 4 {
		data[0] = byte(len(data) - 1)
	} else {
		networkOrder.PutUint16(data, uint16(len(data)-2))
	}
	return data
}
