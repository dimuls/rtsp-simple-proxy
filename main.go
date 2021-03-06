package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aler9/gortsplib"
	"gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/yaml.v2"
)

var Version string = "v0.0.0"

const (
	_READ_TIMEOUT  = 5 * time.Second
	_WRITE_TIMEOUT = 5 * time.Second
)

type trackFlow int

const (
	_TRACK_FLOW_RTP trackFlow = iota
	_TRACK_FLOW_RTCP
)

type track struct {
	rtpPort  int
	rtcpPort int
}

type streamProtocol int

const (
	_STREAM_PROTOCOL_UDP streamProtocol = iota
	_STREAM_PROTOCOL_TCP
)

func (s streamProtocol) String() string {
	if s == _STREAM_PROTOCOL_UDP {
		return "udp"
	}
	return "tcp"
}

type streamConf struct {
	Url    string `yaml:"url"`
	UseTcp bool   `yaml:"useTcp"`
}

type conf struct {
	Protocols          []string
	RtspPort           int
	RtpPort            int
	RtcpPort           int
	StreamReadyTimeout time.Duration
	StreamTTL          time.Duration
}

func loadConf(confPath string) (*conf, error) {
	if confPath == "stdin" {
		var ret conf
		err := yaml.NewDecoder(os.Stdin).Decode(&ret)
		if err != nil {
			return nil, err
		}

		return &ret, nil

	} else {
		f, err := os.Open(confPath)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		var ret conf
		err = yaml.NewDecoder(f).Decode(&ret)
		if err != nil {
			return nil, err
		}

		return &ret, nil
	}
}

type program struct {
	conf      conf
	protocols map[streamProtocol]struct{}
	mutex     sync.RWMutex
	rtspl     *serverTcpListener
	rtpl      *serverUdpListener
	rtcpl     *serverUdpListener
	clients   map[*serverClient]struct{}
	streams   map[string]*stream
}

func newProgram() (*program, error) {
	kingpin.CommandLine.Help = "rtsp-simple-proxy " + Version + "\n\n" +
		"RTSP proxy."

	protocolsStr := kingpin.Flag("protocols", "supported protocols").
		Default("tcp,udp").Envar("PROTOCOLS").String()
	rtspPort := kingpin.Flag("rtsp-port", "port of RTSP TCP listener").
		Default("8554").Envar("RTSP_PORT").Int()
	rtpPort := kingpin.Flag("rtp-port", "port of RTP UDP listener").
		Default("8050").Envar("RTP_PORT").Int()
	rtcpPort := kingpin.Flag("rtcp-port", "port of RTCP UDP listener").
		Default("8051").Envar("RTP_PORT").Int()
	streamReadyTimeout := kingpin.Flag("stream-ready-timeout",
		"timeout to stream become ready in seconds").Default("10s").Duration()
	streamTTL := kingpin.Flag("stream-ttl", "stream without clients time to life in seconds").
		Default("10s").Duration()

	kingpin.Parse()

	conf := &conf{
		Protocols:          strings.Split(*protocolsStr, ","),
		RtspPort:           *rtspPort,
		RtpPort:            *rtpPort,
		RtcpPort:           *rtcpPort,
		StreamReadyTimeout: *streamReadyTimeout,
		StreamTTL:          *streamTTL,
	}

	if conf.RtspPort == 0 {
		return nil, fmt.Errorf("rtsp port not provided")
	}

	if conf.RtpPort == 0 {
		return nil, fmt.Errorf("rtp port not provided")
	}

	if conf.RtcpPort == 0 {
		return nil, fmt.Errorf("rtcp port not provided")
	}

	if (conf.RtpPort % 2) != 0 {
		return nil, fmt.Errorf("rtp port must be even")
	}

	if conf.RtcpPort != (conf.RtpPort + 1) {
		return nil, fmt.Errorf("rtcp port must be rtp port plus 1")
	}

	if conf.StreamReadyTimeout < time.Second {
		return nil, fmt.Errorf("too small stream ready timeout")
	}

	if conf.StreamTTL < time.Second {
		return nil, fmt.Errorf("too small stream TTL")
	}

	protocols := make(map[streamProtocol]struct{})
	for _, proto := range conf.Protocols {
		switch proto {
		case "udp":
			protocols[_STREAM_PROTOCOL_UDP] = struct{}{}

		case "tcp":
			protocols[_STREAM_PROTOCOL_TCP] = struct{}{}

		default:
			return nil, fmt.Errorf("unsupported protocol: %s", proto)
		}
	}
	if len(protocols) == 0 {
		return nil, fmt.Errorf("no protocols provided")
	}

	log.Printf("rtsp-simple-proxy %s", Version)

	p := &program{
		conf:      *conf,
		protocols: protocols,
		clients:   make(map[*serverClient]struct{}),
		streams:   make(map[string]*stream),
	}

	var err error

	p.rtpl, err = newServerUdpListener(p, p.conf.RtpPort, _TRACK_FLOW_RTP)
	if err != nil {
		return nil, err
	}

	p.rtcpl, err = newServerUdpListener(p, p.conf.RtcpPort, _TRACK_FLOW_RTCP)
	if err != nil {
		return nil, err
	}

	p.rtspl, err = newServerTcpListener(p)
	if err != nil {
		return nil, err
	}

	go func() {
		t := time.NewTicker(1 * time.Second)

		streamsClientLastTime := map[string]time.Time{}

		for {
			select {
			case <-t.C:
				p.mutex.Lock()

				for c := range p.clients {
					streamsClientLastTime[c.path] = time.Now()
				}

				for path, lastTime := range streamsClientLastTime {
					if time.Now().Sub(lastTime) >= conf.StreamTTL {
						s, exists := p.streams[path]
						if !exists {
							continue
						}
						s.log("have no clients, stopping")
						close(s.stop)
						delete(p.streams, path)
						delete(streamsClientLastTime, path)
					}
				}

				p.mutex.Unlock()
			}
		}
	}()

	return p, nil
}

func (p *program) run() {
	go p.rtpl.run()
	go p.rtcpl.run()
	go p.rtspl.run()

	infty := make(chan struct{})
	<-infty
}

func (p *program) forwardTrack(path string, id int, flow trackFlow, frame []byte) {
	for c := range p.clients {
		if c.path == path && c.state == _CLIENT_STATE_PLAY {
			if c.streamProtocol == _STREAM_PROTOCOL_UDP {
				if flow == _TRACK_FLOW_RTP {
					p.rtpl.chanWrite <- &udpWrite{
						addr: &net.UDPAddr{
							IP:   c.ip,
							Port: c.streamTracks[id].rtpPort,
						},
						buf: frame,
					}
				} else {
					p.rtcpl.chanWrite <- &udpWrite{
						addr: &net.UDPAddr{
							IP:   c.ip,
							Port: c.streamTracks[id].rtcpPort,
						},
						buf: frame,
					}
				}

			} else {
				c.chanWrite <- &gortsplib.InterleavedFrame{
					Channel: trackToInterleavedChannel(id, flow),
					Content: frame,
				}
			}
		}
	}
}

func main() {
	p, err := newProgram()
	if err != nil {
		log.Fatal("ERR: ", err)
	}

	p.run()
}
