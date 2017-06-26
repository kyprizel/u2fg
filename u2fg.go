package u2fg

import (
    "fmt"
    "log"
    "time"
    "sync"
    "encoding/binary"
    "github.com/kyprizel/hidg"
)

const (
    capabilityWink = 1

    infoProtocolVersion    = 2
    infoMajorDeviceVersion = 2
    infoMinorDeviceVersion = 0
    infoBuildDeviceVersion = 0
    infoRawCapabilities = capabilityWink

    cmdPing  = 0x80 | 0x01
    cmdMsg   = 0x80 | 0x03
    cmdLock  = 0x80 | 0x04
    cmdInit  = 0x80 | 0x06
    cmdWink  = 0x80 | 0x08
    cmdSync  = 0x80 | 0x3c
    cmdError = 0x80 | 0x3f

    errInvalidCmd           = 1
    errInvalidParam         = 2
    errInvalidMsgLen        = 3
    errInvalidMsgSeq        = 4
    errTimeout              = 5
    errChannelBusy          = 6
    errChannelLockRequired  = 7
    errSyncFailed           = 8

    broadcastChannel = 0xffffffff

    hidPacketLen       = 64
    minMessageLen      = 7
    maxMessageLen      = 7609
    minInitResponseLen = 17

    minCid  = 0xdeadbeef
    reqTimeout = 3 * time.Second

    maxChannelLifetime = 120 * time.Second
)


func Init(path string) (*hiDevice, error) {
    hidDev, err := hidg.Open(path)
    if err != nil {
        return nil, err
    }

    d := &hiDevice{
        ProtocolVersion: infoProtocolVersion,
        MajorDeviceVersion: infoMajorDeviceVersion,
        MinorDeviceVersion: infoMinorDeviceVersion,
        BuildDeviceVersion: infoBuildDeviceVersion,
        RawCapabilities: infoRawCapabilities,
        channels: make(map[uint32]Channel),
        lastCid: minCid,
        device: hidDev,
        readCh: hidDev.ReadCh(),
    }

    return d, nil
}


type hiDevice struct {
    ProtocolVersion    uint8
    MajorDeviceVersion uint8
    MinorDeviceVersion uint8
    BuildDeviceVersion uint8
    RawCapabilities uint8

    device  hidg.Device

    mtx    sync.Mutex
    readCh <-chan []byte
    channels    map[uint32]Channel
    buf         []byte
    lastCid     uint32
}


type Channel struct {
    Uid uint32
    Cmd uint8
    Created time.Time
}


type U2FMsgHandler interface {
    ProcessMessage([]byte) []byte
    Wink()
}


func (d *hiDevice) readBuffered() ([]byte, error) {
    timeout := time.After(reqTimeout)

    var buf    []byte
    for {
        select {
        case msg, ok := <-d.readCh:
            if !ok {
                return nil, fmt.Errorf("u2fhidg: error reading response, host closed")
            }

            buf = append(buf, msg...)
            if len(buf) >= hidPacketLen {
                return buf[:hidPacketLen], nil
            }
        case <-timeout:
            return nil, fmt.Errorf("u2fhidg: error reading request, read timed out")
        }
    }
}

func (d *hiDevice) processPackets() ([]byte, error) {
    waitingMore := false
    var buf    []byte
    var msgLen  int
    var lcUid   uint32
    var seq     uint8
    var lseq    uint8

    for {
        packet, err := d.readBuffered()
        if err != nil {
            return nil, err
        }
        chanUid := binary.BigEndian.Uint32(packet)
        if !waitingMore {
            if packet[4] & 0x80 == 0 {
                d.sendResponse(chanUid, cmdError, []byte{errInvalidCmd})
                return nil, fmt.Errorf("u2fhidg: waiting for command, sequence received")
            }
            msgLen = int(binary.BigEndian.Uint16(packet[5:]))
            if msgLen > maxMessageLen {
                d.sendResponse(chanUid, cmdError, []byte{errInvalidMsgLen})
                return nil, fmt.Errorf("u2fhidg: message is too big (%d), ignoring", msgLen)
            }
            buf = make([]byte, 0, msgLen+minMessageLen)
            buf = append(buf, packet...)
            if len(buf) >= msgLen+minMessageLen {
                return buf, nil
            }
            waitingMore = true
            lcUid = chanUid
            lseq = 0
            continue
        }
        if lcUid != chanUid {
            d.sendResponse(chanUid, cmdError, []byte{errInvalidParam})
            return nil, fmt.Errorf("u2fhidg: wrong channel id (%x), ignoring", chanUid)
        }
        seq = packet[4]
        if seq < lseq {
            d.sendResponse(chanUid, cmdError, []byte{errInvalidMsgSeq})
            return nil, fmt.Errorf("u2fhidg: wrong sequence number (%d), breaking", seq)
        }
        buf = append(buf, packet[5:]...)
        lseq = seq
        if len(buf) >= msgLen+minMessageLen {
           return buf, nil
        }
    }
}


func (d *hiDevice) Run(h U2FMsgHandler) error {
    d.buf = make([]byte, hidPacketLen)

    for {
        packet, err := d.processPackets()
        if err != nil {
//            log.Printf("Err: %v", err)
            continue
        }
        chanUid := binary.BigEndian.Uint32(packet)
        cmd := packet[4]
        if chanUid == broadcastChannel {
            log.Printf("Using broadcast channel")
            switch cmd {
                case cmdPing:
                    log.Printf("got PING_CMD")
                    d.sendResponse(chanUid, cmd, packet[5:])
                case cmdInit:
                    log.Printf("got INIT_CMD")
                    log.Printf("allocating channel")
                    c := Channel{Uid: d.AllocateChannel(d.lastCid), Cmd: cmd, Created: time.Now()}
                    resp := make([]byte, 17)
                    _ = copy(resp[0:], packet[7:15])
                    binary.BigEndian.PutUint32(resp[8:], c.Uid)
                    resp[12] = d.ProtocolVersion
                    resp[13] = d.MajorDeviceVersion
                    resp[14] = d.MinorDeviceVersion
                    resp[15] = d.BuildDeviceVersion
                    resp[16] = d.RawCapabilities
                    d.channels[c.Uid] = c
                    d.sendResponse(chanUid, cmd, resp)
                case cmdSync:
                    log.Printf("got SYNC_CMD")
                    log.Printf("Not sure what to do, ignoring")
                default:
                    d.sendResponse(chanUid, cmdError, []byte{errInvalidCmd})
            }
        } else {
            log.Printf("Using channel: %x", chanUid)
            c, isValid := d.channels[chanUid]
            if isValid {
                switch cmd {
                    case cmdPing:
                        log.Printf("got PING_CMD")
                        d.sendResponse(c.Uid, cmd, packet[5:])
                    case cmdMsg:
                        log.Printf("got MSG_CMD")
                        response := h.ProcessMessage(packet[7:])
                        d.sendResponse(c.Uid, cmd, response)
                    case cmdSync:
                        log.Printf("got SYNC_CMD")
                        log.Printf("Not sure what to do, ignoring")
                    case cmdWink:
                        log.Printf("got WINK_CMD")
                        h.Wink()
                        d.sendResponse(chanUid, cmd, nil)
                    default:
                        d.sendResponse(chanUid, cmdError, []byte{errInvalidCmd})
                }
            } else {
                log.Printf("Invalid channel id (%x), ignoring", chanUid)
                d.sendResponse(chanUid, cmdError, []byte{errInvalidParam})
            }
        }
    }
    return nil
}


func (d *hiDevice) Close() {
    d.device.Close()
}


func (d *hiDevice) sendResponse(channel uint32, cmd byte, data []byte) error {
    if len(data) > maxMessageLen {
        return fmt.Errorf("u2fhid: message is too long")
    }

    // zero buffer
    for i := range d.buf {
        d.buf[i] = 0
    }

    binary.BigEndian.PutUint32(d.buf[0:], channel)
    d.buf[4] = cmd
    binary.BigEndian.PutUint16(d.buf[5:], uint16(len(data)))

    n := copy(d.buf[7:], data)
    data = data[n:]

    if err := d.device.Write(d.buf); err != nil {
        return err
    }

    var seq uint8
    for len(data) > 0 {
        for i := range d.buf {
            d.buf[i] = 0
        }

        binary.BigEndian.PutUint32(d.buf[0:], channel)
        d.buf[4] = seq
        seq++
        n := copy(d.buf[5:], data)
        data = data[n:]
        if err := d.device.Write(d.buf); err != nil {
            return err
        }
    }
    return nil
}


func (d *hiDevice) AllocateChannel(lastCid uint32) uint32 {
    var newCid uint32

    if lastCid + 1 < broadcastChannel {
        newCid = lastCid + 1
    } else {
        newCid = minCid
    }
    c, existing := d.channels[newCid]
    if existing {
        now := time.Now()
        if now.Sub(c.Created) > maxChannelLifetime {
            return newCid
        }
        return d.AllocateChannel(newCid + 1)
    }
    return newCid
}
