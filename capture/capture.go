package capture

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/buger/goreplay/size"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
)

// TransportLayers current supported transport layers
var TransportLayers = [...]string{
	"tcp",
}

// Handler is a function that is used to handle packets
type Handler func(gopacket.Packet)

// PcapOptions options that can be set on a pcap capture handle,
// these options take effect on inactive pcap handles
type PcapOptions struct {
	Promiscuous   bool          `json:"input-raw-promisc"`
	Monitor       bool          `json:"input-raw-monitor"`
	Snaplen       bool          `json:"input-raw-override-snaplen"`
	BufferTimeout time.Duration `json:"input-raw-buffer-timeout"`
	TimestampType string        `json:"input-raw-timestamp-type"`
	BufferSize    size.Size     `json:"input-raw-buffer-size"`
	BPFFilter     string        `json:"input-raw-bpf-filter"`
}

// NetInterface represents network interface
type NetInterface struct {
	net.Interface
	IPs []string
}

// Listener handle traffic capture, this is its representation.
type Listener struct {
	sync.Mutex
	PcapOptions
	Engine     EngineType
	Transport  string       // transport layer default to tcp
	Activate   func() error // function is used to activate the engine. it must be called before reading packets
	Handles    map[string]*pcap.Handle
	Interfaces []NetInterface
	Reading    chan bool // this channel is closed when the listener has started reading packets

	host          string // pcap file name or interface (name, hardware addr, index or ip address)
	port          uint16 // src or/and dst port
	trackResponse bool

	quit    chan bool
	packets chan gopacket.Packet
}

// EngineType ...
type EngineType uint8

// Available engines for intercepting traffic
const (
	EnginePcap EngineType = iota
	EnginePcapFile
)

// Set is here so that EngineType can implement flag.Var
func (eng *EngineType) Set(v string) error {
	switch v {
	case "", "libcap":
		*eng = EnginePcap
	case "pcap_file":
		*eng = EnginePcapFile
	default:
		return fmt.Errorf("invalid engine %s", v)
	}
	return nil
}

func (eng *EngineType) String() (e string) {
	switch *eng {
	case EnginePcapFile:
		e = "pcap_file"
	case EnginePcap:
		e = "libpcap"
	default:
		e = ""
	}
	return e
}

// NewListener creates and initialize a new Listener. if transport or/and engine are invalid/unsupported
// is "tcp" and "pcap", are assumed. l.Engine and l.Transport can help to get the values used.
// if there is an error it will be associated with getting network interfaces
func NewListener(host string, port uint16, transport string, engine EngineType, trackResponse bool) (l *Listener, err error) {
	l = &Listener{}

	l.host = host
	l.port = port
	l.Transport = "tcp"
	if transport != "" {
		for _, v := range TransportLayers {
			if v == transport {
				l.Transport = transport
				break
			}
		}
	}
	l.Handles = make(map[string]*pcap.Handle)
	l.trackResponse = trackResponse
	l.packets = make(chan gopacket.Packet, 1000)
	l.quit = make(chan bool, 1)
	l.Reading = make(chan bool, 1)
	l.Activate = l.activatePcap
	l.Engine = EnginePcap
	if engine == EnginePcapFile {
		l.Activate = l.activatePcapFile
		l.Engine = EnginePcapFile
		return
	}
	err = l.setInterfaces()
	if err != nil {
		return nil, err
	}
	return
}

// SetPcapOptions set pcap options for all yet to be actived pcap handles
// setting this on already activated handles will not have any effect
func (l *Listener) SetPcapOptions(opts PcapOptions) {
	l.PcapOptions = opts
}

// Listen listens for packets from the handles, and call handler on every packet received
// until the context done signal is sent or EOF on handles.
// this function should be called after activating pcap handles
func (l *Listener) Listen(ctx context.Context, handler Handler) (err error) {
	l.read()
	done := ctx.Done()
	var p gopacket.Packet
	var ok bool
	l.Reading <- true
	close(l.Reading)
	for {
		select {
		case <-done:
			l.quit <- true
			close(l.quit)
			err = ctx.Err()
			return
		case p, ok = <-l.packets:
			if !ok {
				return
			}
			if p == nil {
				continue
			}
			handler(p)
		}
	}
}

// ListenBackground is like listen but can run concurrently and signal error through channel
func (l *Listener) ListenBackground(ctx context.Context, handler Handler) chan error {
	err := make(chan error, 1)
	go func() {
		defer close(err)
		if e := l.Listen(ctx, handler); err != nil {
			err <- e
		}
	}()
	return err
}

// Filter returns automatic filter applied by goreplay
// to a pcap handle of a specific interface
func (l *Listener) Filter(ifi NetInterface) (filter string) {
	// https://www.tcpdump.org/manpages/pcap-filter.7.html

	port := fmt.Sprintf("portrange 0-%d", 1<<16-1)
	if l.port != 0 {
		port = fmt.Sprintf("port %d", l.port)
	}
	dir := " dst " // direction
	if l.trackResponse {
		dir = " "
	}
	filter = fmt.Sprintf("(%s%s%s)", l.Transport, dir, port)
	if l.host == "" || isDevice(l.host, ifi) {
		return
	}
	filter = fmt.Sprintf("(%s%s%s and host %s)", l.Transport, dir, port, l.host)
	return
}

// PcapDumpHandler returns a handler to write packet data in PCAP
// format, See http://wiki.wireshark.org/Development/LibpcapFileFormathandler.
// if link layer is invalid Ethernet is assumed
func PcapDumpHandler(file *os.File, link layers.LinkType, debugger func(int, ...interface{})) (handler func(packet gopacket.Packet), err error) {
	if link.String() == "" {
		link = layers.LinkTypeEthernet
	}
	w := NewWriterNanos(file)
	err = w.WriteFileHeader(64<<10, link)
	if err != nil {
		return nil, err
	}
	return func(packet gopacket.Packet) {
		err = w.WritePacket(packet.Metadata().CaptureInfo, packet.Data())
		if err != nil && debugger != nil {
			go debugger(3, err)
		}
	}, nil
}

// PcapHandle returns new pcap Handle from dev on success.
// this function should be called after setting all necessary options for this listener
func (l *Listener) PcapHandle(ifi NetInterface) (handle *pcap.Handle, err error) {
	var inactive *pcap.InactiveHandle
	inactive, err = pcap.NewInactiveHandle(ifi.Name)
	if inactive != nil && err != nil {
		defer inactive.CleanUp()
	}
	if err != nil {
		return nil, fmt.Errorf("inactive handle error: %q, interface: %q", err, ifi.Name)
	}
	if l.TimestampType != "" {
		var ts pcap.TimestampSource
		ts, err = pcap.TimestampSourceFromString(l.TimestampType)
		err = inactive.SetTimestampSource(ts)
		if err != nil {
			return nil, fmt.Errorf("%q: supported timestamps: %q, interface: %q", err, inactive.SupportedTimestamps(), ifi.Name)
		}
	}
	if l.Promiscuous {
		if err = inactive.SetPromisc(l.Promiscuous); err != nil {
			return nil, fmt.Errorf("promiscuous mode error: %q, interface: %q", err, ifi.Name)
		}
	}
	if l.Monitor {
		if err = inactive.SetRFMon(l.Monitor); err != nil && !errors.Is(err, pcap.CannotSetRFMon) {
			return nil, fmt.Errorf("monitor mode error: %q, interface: %q", err, ifi.Name)
		}
	}
	var snap int
	if l.Snaplen {
		snap = 64<<10 + 200
	} else if ifi.MTU > 0 {
		snap = ifi.MTU + 200
	}
	err = inactive.SetSnapLen(snap)
	if err != nil {
		return nil, fmt.Errorf("snapshot length error: %q, interface: %q", err, ifi.Name)
	}
	if l.BufferSize > 0 {
		err = inactive.SetBufferSize(int(l.BufferSize))
		if err != nil {
			return nil, fmt.Errorf("handle buffer size error: %q, interface: %q", err, ifi.Name)
		}
	}
	if l.BufferTimeout.Nanoseconds() == 0 {
		l.BufferTimeout = pcap.BlockForever
	}
	err = inactive.SetTimeout(l.BufferTimeout)
	if err != nil {
		return nil, fmt.Errorf("handle buffer timeout error: %q, interface: %q", err, ifi.Name)
	}
	handle, err = inactive.Activate()
	if err != nil {
		return nil, fmt.Errorf("PCAP Activate device error: %q, interface: %q", err, ifi.Name)
	}
	if l.BPFFilter != "" {
		if l.BPFFilter[0] != '(' {
			l.BPFFilter = "(" + l.BPFFilter
		}
		if l.BPFFilter[len(l.BPFFilter)-1] != ')' {
			l.BPFFilter += ")"
		}
	} else {
		l.BPFFilter = l.Filter(ifi)
	}
	err = handle.SetBPFFilter(l.BPFFilter)
	if err != nil {
		handle.Close()
		return nil, fmt.Errorf("BPF filter error: %q%s, interface: %q", err, l.BPFFilter, ifi.Name)
	}
	return
}

func (l *Listener) read() {
	l.Lock()
	defer l.Unlock()
	for key, handle := range l.Handles {
		source := gopacket.NewPacketSource(handle, handle.LinkType())
		source.Lazy = true
		source.NoCopy = true
		ch := source.Packets()
		go func(handle *pcap.Handle, key string) {
			defer l.closeHandle(key)
			for {
				select {
				case <-l.quit:
					return
				case p, ok := <-ch:
					if !ok {
						return
					}
					l.packets <- p
				}
			}
		}(handle, key)
	}
}

func (l *Listener) closeHandle(key string) {
	l.Lock()
	defer l.Unlock()
	if handle, ok := l.Handles[key]; ok {
		handle.Close()
		delete(l.Handles, key)
		if len(l.Handles) == 0 {
			close(l.packets)
		}
	}
}

func (l *Listener) activatePcap() (err error) {
	var e error
	var msg string
	for _, ifi := range l.Interfaces {
		var handle *pcap.Handle
		handle, e = l.PcapHandle(ifi)
		if e != nil {
			msg += ("\n" + e.Error())
			continue
		}
		l.Handles[ifi.Name] = handle
	}
	if len(l.Handles) == 0 {
		return fmt.Errorf("pcap handles error:%s", msg)
	}
	return
}

func (l *Listener) activatePcapFile() (err error) {
	var handle *pcap.Handle
	var e error
	if handle, e = pcap.OpenOffline(l.host); e != nil {
		return fmt.Errorf("open pcap file error: %q", e)
	}
	if l.BPFFilter != "" {
		if l.BPFFilter[0] != '(' {
			l.BPFFilter = "(" + l.BPFFilter
		}
		if l.BPFFilter[len(l.BPFFilter)-1] != ')' {
			l.BPFFilter += ")"
		}
	} else {
		addr := l.host
		l.host = ""
		l.BPFFilter = l.Filter(NetInterface{})
		l.host = addr
	}
	if e = handle.SetBPFFilter(l.BPFFilter); e != nil {
		handle.Close()
		return fmt.Errorf("BPF filter error: %q, filter: %s", e, l.BPFFilter)
	}
	l.Handles["pcap_file"] = handle
	return
}

func (l *Listener) setInterfaces() (err error) {
	var Ifis []NetInterface
	var ifis []net.Interface
	ifis, err = net.Interfaces()
	if err != nil {
		return err
	}

	for i := 0; i < len(ifis); i++ {
		if ifis[i].Flags&net.FlagUp == 0 {
			continue
		}
		var addrs []net.Addr
		addrs, err = ifis[i].Addrs()
		if err != nil {
			return err
		}
		if len(addrs) == 0 {
			continue
		}
		ifi := NetInterface{}
		ifi.Interface = ifis[i]
		ifi.IPs = make([]string, len(addrs))
		for j, addr := range addrs {
			ifi.IPs[j] = cutMask(addr)
		}
		Ifis = append(Ifis, ifi)
	}

	switch l.host {
	case "", "0.0.0.0", "[::]", "::":
		l.Interfaces = Ifis
		return
	}

	found := false
	for _, ifi := range Ifis {
		if l.host == ifi.Name || l.host == fmt.Sprintf("%d", ifi.Index) || l.host == ifi.HardwareAddr.String() {
			found = true
		}
		for _, ip := range ifi.IPs {
			if ip == l.host {
				found = true
				break
			}
		}
		if found {
			l.Interfaces = []NetInterface{ifi}
			return
		}
	}
	err = fmt.Errorf("can not find interface with addr, name or index %s", l.host)
	return err
}

func cutMask(addr net.Addr) string {
	mask := addr.String()
	for i, v := range mask {
		if v == '/' {
			return mask[:i]
		}
	}
	return mask
}

func isDevice(addr string, ifi NetInterface) bool {
	return addr == ifi.Name || addr == fmt.Sprintf("%d", ifi.Index) || addr == ifi.HardwareAddr.String()
}
