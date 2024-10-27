package player

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/librespot-org/librespot-golang/Spotify"
	"github.com/librespot-org/librespot-golang/librespot/connection"
	"github.com/librespot-org/librespot-golang/librespot/mercury"
)

type Player struct {
	stream   connection.PacketStream
	mercury  *mercury.Client
	seq      uint32
	audioKey []byte

	chanLock    sync.Mutex
	seqChanLock sync.Mutex
	channels    map[uint16]*Channel
	seqChans    sync.Map
	nextChan    uint16

	hadPacketErr bool

	ctx        context.Context
	cancelFunc context.CancelFunc

	isRateLimited    bool
	rateLimitedUntil time.Time
	rateLimitMutex   sync.RWMutex
}

func (p *Player) IsRateLimited() bool {
	p.rateLimitMutex.RLock()
	defer p.rateLimitMutex.RUnlock()
	return p.isRateLimited && time.Now().Before(p.rateLimitedUntil)
}

func (p *Player) setRateLimit(duration time.Duration) {
	p.rateLimitMutex.Lock()
	defer p.rateLimitMutex.Unlock()
	p.isRateLimited = true
	p.rateLimitedUntil = time.Now().Add(duration)
}

func (p *Player) clearRateLimit() {
	p.rateLimitMutex.Lock()
	defer p.rateLimitMutex.Unlock()
	p.isRateLimited = false
}

func CreatePlayer(conn connection.PacketStream, client *mercury.Client) *Player {
	ctx, cancel := context.WithCancel(context.Background())
	return &Player{
		stream:       conn,
		mercury:      client,
		channels:     map[uint16]*Channel{},
		seqChans:     sync.Map{},
		chanLock:     sync.Mutex{},
		nextChan:     0,
		hadPacketErr: false,
		ctx:          ctx,
		cancelFunc:   cancel,
	}
}

func (p *Player) Reset() {
	// newPlayer := CreatePlayer(p.stream, p.mercury)
	// // assign the value of newPlayer to p
	// *p = *newPlayer

	// Cancel all ongoing operations
	p.chanLock.Lock()
	for _, channel := range p.channels {
		p.releaseChannel(channel)
	}
	p.chanLock.Unlock()

	// Clear all pending sequences
	p.seqChans.Range(func(key, value interface{}) bool {
		ch := value.(chan []byte)
		close(ch)
		p.seqChans.Delete(key)
		return true
	})

}

func (p *Player) HadPacketError() bool {
	return p.hadPacketErr
}

func (p *Player) LoadTrack(file *Spotify.AudioFile, trackId []byte) (*AudioFile, error) {
	// return p.LoadTrackWithIdAndFormat(file.FileId, file.GetFormat(), trackId)
	select {
	case <-p.ctx.Done():
		return nil, fmt.Errorf("player stopped")
	default:
		return p.LoadTrackWithIdAndFormat(file.FileId, file.GetFormat(), trackId)
	}
}

type loadConfig struct {
	onChunk func(int)
}

type LoadTrackConfig func(*loadConfig)

func OnChunk(f func(int)) LoadTrackConfig {
	return func(c *loadConfig) {
		c.onChunk = f
	}
}

func (p *Player) LoadTrackWithIdAndFormat(fileId []byte, format Spotify.AudioFile_Format, trackId []byte, opt ...LoadTrackConfig) (*AudioFile, error) {
	select {
	case <-p.ctx.Done():
		return nil, fmt.Errorf("player stopped")
	default:
		if p.IsRateLimited() {
			return nil, fmt.Errorf("rate limited, try again after %v", p.rateLimitedUntil)
		}

		cfg := &loadConfig{}
		for _, o := range opt {
			o(cfg)
		}

		audioFile := newAudioFileWithIdAndFormat(fileId, format, p)

		if cfg.onChunk != nil {
			audioFile.onChunk = cfg.onChunk
		}

		err := audioFile.loadKey(trackId)
		if err != nil {
			if strings.Contains(err.Error(), "rate limited") {
				// Assume a default rate limit duration if not specified
				duration := 60 * time.Second
				p.setRateLimit(duration)
				return nil, fmt.Errorf("rate limited, try again after %v", duration)
			}
			return nil, err
		}

		audioFile.loadChunks()
		p.clearRateLimit() // Clear rate limit after successful load
		return audioFile, nil
	}
}

func (p *Player) Stop() {
	p.cancelFunc()

	// Cancel all ongoing operations
	p.chanLock.Lock()
	for _, channel := range p.channels {
		p.releaseChannel(channel)
	}
	p.chanLock.Unlock()

	// Clear all pending sequences
	p.seqChans.Range(func(key, value interface{}) bool {
		ch := value.(chan []byte)
		close(ch)
		p.seqChans.Delete(key)
		return true
	})

	log.Println("Player stopped")
}

// func (p *Player) LoadTrackWithIdAndFormat(fileId []byte, format Spotify.AudioFile_Format, trackId []byte) (*AudioFile, error) {
// 	// fmt.Printf("[player] Loading track audio key, fileId: %s, trackId: %s\n", utils.ConvertTo62(fileId), utils.ConvertTo62(trackId))

// 	// Allocate an AudioFile and a channel
// 	audioFile := newAudioFileWithIdAndFormat(fileId, format, p)

// 	// Start loading the audio key
// 	err := audioFile.loadKey(trackId)

// 	// Then start loading the audio itself
// 	audioFile.loadChunks()

// 	return audioFile, err
// }

// func (p *Player) loadTrackKey(trackId []byte, fileId []byte) ([]byte, error) {
// 	seqInt, seq := p.mercury.NextSeqWithInt()

// 	p.seqChans.Store(seqInt, make(chan []byte))

// 	req := buildKeyRequest(seq, trackId, fileId)
// 	err := p.stream.SendPacket(connection.PacketRequestKey, req)
// 	if err != nil {
// 		log.Println("Error while sending packet", err)
// 		return nil, err
// 	}

// 	channel, _ := p.seqChans.Load(seqInt)
// 	key := <-channel.(chan []byte)
// 	p.seqChans.Delete(seqInt)

// 	return key, nil
// }

func (p *Player) loadTrackKey(trackId []byte, fileId []byte) ([]byte, error) {
	if p.IsRateLimited() {
		return nil, fmt.Errorf("rate limited, try again after %v", p.rateLimitedUntil)
	}

	seqInt, seq := p.mercury.NextSeqWithInt()
	p.seqChans.Store(seqInt, make(chan []byte))

	req := buildKeyRequest(seq, trackId, fileId)
	err := p.stream.SendPacket(connection.PacketRequestKey, req)
	if err != nil {
		// Check if the error indicates rate limiting
		if strings.Contains(err.Error(), "rate limited") {
			// Assume a default rate limit duration if not specified
			duration := 60 * time.Second
			p.setRateLimit(duration)
			return nil, fmt.Errorf("rate limited, try again after %v", duration)
		}
		return nil, err
	}

	channel, _ := p.seqChans.Load(seqInt)
	key := <-channel.(chan []byte)
	p.seqChans.Delete(seqInt)

	p.clearRateLimit() // Clear rate limit after successful request
	return key, nil
}

func (p *Player) AllocateChannel() *Channel {
	p.chanLock.Lock()
	channel := NewChannel(p.nextChan, p.releaseChannel)
	p.nextChan++

	p.channels[channel.num] = channel
	p.chanLock.Unlock()

	return channel
}

func (p *Player) HandleCmd(cmd byte, data []byte) {
	switch {
	case cmd == connection.PacketAesKey:
		// Audio key response
		dataReader := bytes.NewReader(data)
		var seqNum uint32
		binary.Read(dataReader, binary.BigEndian, &seqNum)

		if channel, ok := p.seqChans.Load(seqNum); ok {
			channel.(chan []byte) <- data[4:20]
		} else {
			fmt.Printf("[player] Unknown channel for audio key seqNum %d\n", seqNum)
		}

	case cmd == connection.PacketAesKeyError:
		// Audio key error
		// fmt.Println("[player] Audio key error!")
		// fmt.Printf("%x\n", data)
		p.hadPacketErr = true

	case cmd == connection.PacketStreamChunkRes:
		// Audio data response
		var channel uint16
		dataReader := bytes.NewReader(data)
		binary.Read(dataReader, binary.BigEndian, &channel)

		// fmt.Printf("[player] Data on channel %d: %d bytes\n", channel, len(data[2:]))

		if val, ok := p.channels[channel]; ok {
			val.handlePacket(data[2:])
		} else {
			fmt.Printf("Unknown channel!\n")
		}
	}
}

func (p *Player) releaseChannel(channel *Channel) {
	p.chanLock.Lock()
	delete(p.channels, channel.num)
	p.chanLock.Unlock()
	// fmt.Printf("[player] Released channel %d\n", channel.num)
}
