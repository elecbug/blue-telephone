package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	"github.com/libp2p/go-libp2p/p2p/muxer/yamux"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	"github.com/multiformats/go-multiaddr"
)

const NameExchangeProtocol = "/blue-telephone/name-exchange/1.0.0"

func main() {
	group, name, port := CreateFlag()

	conn, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", port))

	if err != nil {
		log.Fatalln(err)
	}

	ctx := context.Background()
	friends := []PeerName{}
	topics := []Topic{}

	_, gossip := CreateHostAndExchangeInfo(ctx, group, name, friends, conn)

	for {
		buf := make([]byte, 1024)

		_, err := io.ReadFull(conn, buf)

		if err != nil {
			log.Println(err)
			WritePacket(conn, DeniedError, []string{err.Error()})
			continue
		}

		var packet Packet

		err = json.Unmarshal([]byte(strings.TrimRight(string(buf), "\x00")), &packet)

		if err != nil {
			log.Println(err)
			WritePacket(conn, DeniedError, []string{err.Error()})
			continue
		}

		switch packet.MsgCode {
		case JoinGossip:
			topic, err := gossip.Join(packet.Msg[0])

			if err != nil {
				log.Println(err)
				WritePacket(conn, DeniedError, []string{err.Error()})
				continue
			}
			sub, err := topic.Subscribe()

			if err != nil {
				log.Println(err)
				WritePacket(conn, DeniedError, []string{err.Error()})
				continue
			}

			topicCtx, cancelCtx := context.WithCancel(ctx)

			topics = append(topics, Topic{
				topic:  topic,
				sub:    sub,
				ctx:    topicCtx,
				cancel: cancelCtx,
			})

			go func() {
				for {
					msg, err := sub.Next(topicCtx)

					if err != nil {
						if err.Error() != "context canceled" {
							log.Println(err)
							WritePacket(conn, DeniedError, []string{err.Error()})
						} else {
							log.Println(err)
							WritePacket(conn, Success, []string{err.Error()})

							break
						}
					} else {
						log.Println("got msg", string(msg.Data), "from", msg.ReceivedFrom.String(), "in topic", topic.String())
						WritePacket(conn, GotGossip, []string{topic.String(), msg.ReceivedFrom.String(), string(msg.Data)})
					}
				}
			}()

			log.Println("Success joins topic")
			WritePacket(conn, Success, []string{"Success joins topic"})

		case ExitGossip:
			for i, v := range topics {
				if v.topic.String() == packet.Msg[0] {
					v.cancel()
					v.sub.Cancel()
					err = v.topic.Close()

					if err != nil {
						log.Println(err)
						WritePacket(conn, DeniedError, []string{err.Error()})
						continue
					}

					topics = append(topics[:i], topics[i+1:]...)

					log.Println("Success exits topic")
					WritePacket(conn, Success, []string{"Success exits topic"})
				}
			}

		case Publish:
			for _, v := range topics {
				if v.topic.String() == packet.Msg[0] {
					err = v.topic.Publish(ctx, []byte(packet.Msg[1]))

					if err != nil {
						log.Println(err)
						WritePacket(conn, DeniedError, []string{err.Error()})
						continue
					}

					log.Println("Success publish topic")
					WritePacket(conn, Success, []string{"Success publish topic"})
				}
			}
		}
	}
}

func CreateFlag() (string, string, int) {
	group := flag.String("group", "default", "group(mdns rendezvous)")
	name := flag.String("name", fmt.Sprintf("BT-%d", rand.Int()), "user nick name")
	port := flag.Int("port", 12000, "local port")

	flag.Parse()

	return *group, *name, *port
}

func CreateHostAndExchangeInfo(ctx context.Context, rendezvous string, name string, friends []PeerName, conn net.Conn) (host.Host, *pubsub.PubSub) {
	host, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/0"),
		libp2p.Security(noise.ID, noise.New),
		libp2p.Muxer(yamux.ID, yamux.DefaultTransport),
		libp2p.Transport(tcp.NewTCPTransport),
	)

	if err != nil {
		log.Fatalln(err)
		WritePacket(conn, PanicError, []string{err.Error()})
	} else {
		addrs := make([]string, len(host.Addrs()))

		for i, v := range host.Addrs() {
			addrs[i] = v.String()
		}

		log.Println("Self:", host.Addrs(), host.ID())
		WritePacket(conn, CreateHost, append(addrs, host.ID().String()))
	}

	ps, err := pubsub.NewGossipSub(ctx, host)

	if err != nil {
		log.Fatalln(err)
		WritePacket(conn, PanicError, []string{err.Error()})
	}

	peerChan := InitMDNS(host, rendezvous, conn)

	go func() {
		host.SetStreamHandler(protocol.ID(NameExchangeProtocol), func(stream network.Stream) {
			buf := make([]byte, 1024)
			stream.Read(buf)

			buf = bytes.Trim(buf, "\x00")

			friends = append(friends, PeerName{
				peer.AddrInfo{
					ID:    stream.Conn().RemotePeer(),
					Addrs: []multiaddr.Multiaddr{stream.Conn().RemoteMultiaddr()},
				},
				string(buf),
			})

			log.Println("Add:", stream.Conn().RemoteMultiaddr().String(), stream.Conn().RemotePeer(), string(buf))
			WritePacket(conn, FoundPeer, []string{stream.Conn().RemoteMultiaddr().String(), stream.Conn().RemotePeer().String(), string(buf)})
		})

		for {
			peer := <-peerChan

			err = host.Connect(ctx, peer)

			if err != nil {
				log.Println(err)
				WritePacket(conn, DeniedError, []string{err.Error()})
				continue
			}

			stream, err := host.NewStream(ctx, peer.ID, protocol.ID(NameExchangeProtocol))

			if err != nil {
				log.Println(err)
				WritePacket(conn, DeniedError, []string{err.Error()})
				continue
			}

			_, err = stream.Write([]byte(name))

			if err != nil {
				log.Println(err)
				WritePacket(conn, DeniedError, []string{err.Error()})
				continue
			}

			err = stream.Close()

			if err != nil {
				log.Println(err)
				WritePacket(conn, DeniedError, []string{err.Error()})
				continue
			}
		}
	}()

	go func() {
		for {
			time.Sleep(10 * time.Second)

			for i, v := range friends {
				err := host.Connect(ctx, v.info)

				if err != nil {
					log.Println("Remove:", v.info.ID)
					WritePacket(conn, RemovePeer, []string{v.info.ID.String()})

					friends = append(friends[:i], friends[i+1:]...)

					continue
				}
			}
		}
	}()

	return host, ps
}

func WritePacket(conn net.Conn, msgCode int, msg []string) {
	packet := &Packet{
		TS:      time.Now().String(),
		MsgCode: msgCode,
		Msg:     msg,
	}

	json, err := json.Marshal(packet)

	if err != nil {
		log.Fatalln(err)
	}

	buf := make([]byte, 1024)
	copy(buf, []byte(string(json)))

	if len(buf) > 1024 {
		WritePacket(conn, DeniedError, []string{"packet size over 1024"})
	} else {
		_, err = conn.Write(buf)

		if err != nil {
			log.Fatalln(err)
		}
	}
}

type PeerName struct {
	info peer.AddrInfo
	name string
}

type DiscoveryNotifee struct {
	PeerChan chan peer.AddrInfo
}

type Packet struct {
	TS      string
	MsgCode int
	Msg     []string
}

type Topic struct {
	topic  *pubsub.Topic
	sub    *pubsub.Subscription
	ctx    context.Context
	cancel context.CancelFunc
}

func (n *DiscoveryNotifee) HandlePeerFound(pi peer.AddrInfo) {
	n.PeerChan <- pi
}

func InitMDNS(peerhost host.Host, rendezvous string, conn net.Conn) chan peer.AddrInfo {
	n := &DiscoveryNotifee{}
	n.PeerChan = make(chan peer.AddrInfo)

	ser := mdns.NewMdnsService(peerhost, rendezvous, n)

	err := ser.Start()

	if err != nil {
		log.Fatalln(err)
		WritePacket(conn, PanicError, []string{err.Error()})
	}

	return n.PeerChan
}
