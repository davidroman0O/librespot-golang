package spotcontrol

import (
	"bytes"
	"fmt"
	Spotify "github.com/badfortrains/spotcontrol/proto"
	"github.com/golang/protobuf/proto"
	"io/ioutil"
	"encoding/base64"
	"log"
	"net"
)

const (
	SUBSCRIBE = iota
	REQUEST
)

type command struct {
	commandType uint32
	uri         string
	responseCh  chan MercuryResponse
	request     MercuryRequest
}

type Session struct {
	stream  ShannonStream
	mercury MercuryManager

	mercuryCommands chan command
	discovery discovery
}

func (s *Session) StartConnection() {
	tcpCon, err := net.Dial("tcp", "sjc1-accesspoint-a95.ap.spotify.com:4070")
	if err != nil {
		log.Fatal("Failed to coonect:", err)
	}
	conn := MakePlainConnection(tcpCon, tcpCon)

	keys := GenerateKeys()
	helloMessage := helloPacket(keys.pubKey())
	initClientPacket, err := conn.SendPrefixPacket([]byte{0, 4}, helloMessage)
	if err != nil {
		log.Fatal("error writing client hello ", err)
	}

	initServerPacket, _ := conn.RecvPacket()
	response := &Spotify.APResponseMessage{}
	err = proto.Unmarshal(initServerPacket[4:], response)
	if err != nil {
		log.Fatal("failed to Unmarshal server packet")
	}

	remoteKey := response.Challenge.LoginCryptoChallenge.DiffieHellman.Gs
	sharedKeys := keys.addRemoteKey(remoteKey, initClientPacket, initServerPacket)

	plainResponse := &Spotify.ClientResponsePlaintext{
		LoginCryptoResponse: &Spotify.LoginCryptoResponseUnion{
			DiffieHellman: &Spotify.LoginCryptoDiffieHellmanResponse{
				Hmac: sharedKeys.challenge,
			},
		},
		PowResponse:    &Spotify.PoWResponseUnion{},
		CryptoResponse: &Spotify.CryptoResponseUnion{},
	}

	plainResponsMessage, err := proto.Marshal(plainResponse)
	if err != nil {
		log.Fatal("marshaling error: ", err)
	}

	_, err = conn.SendPrefixPacket([]byte{}, plainResponsMessage)
	if err != nil {
		log.Fatal("error writing client plain response ", err)
	}

	s.stream = SetupStream(sharedKeys, conn)
	s.mercury = SetupMercury(s)
	s.mercuryCommands = make(chan command)
}

func (s *Session) doLogin(packet []byte) {
	err := s.stream.SendPacket(0xab, packet)
	if err != nil {
		log.Fatal("bad shannon write", err)
	}

	//poll once for authentication response
	s.Poll()
}

func Login(username string, password string, appkeyPath string) *Session {
	s := Session{}
	s.StartConnection()
	loginPacket := loginPacketPassword(appkeyPath, username, password)
	s.doLogin(loginPacket)
	return &s
}

func LoginDiscovery(cacheBlobPath, appkeyPath string) *Session {
	discovery := LoginFromConnect(cacheBlobPath);
	s := Session{
		discovery: discovery,
	}
	s.StartConnection()
	loginPacket := getLoginBlobPacket(appkeyPath, discovery.loginBlob)
	s.doLogin(loginPacket)
	return &s
}

func LoginBlobFile(cacheBlobPath, appkeyPath string) *Session {
	discovery := LoginFromFile(cacheBlobPath);
	s := Session{
		discovery: discovery,
	}
	s.StartConnection()
	loginPacket := getLoginBlobPacket(appkeyPath, discovery.loginBlob)
	s.doLogin(loginPacket)
	return &s
}

type cmdPkt struct {
	cmd  uint8
	data []byte
}

func (s *Session) Run() {
	pktCh := make(chan cmdPkt)
	done := make(chan int)

	//wrap RecvPacket in a goroutine so we can select on it
	go func() {
		for {
			cmd, data, err := s.stream.RecvPacket()
			if err != nil {
				log.Fatal(err)
			}
			pktCh <- cmdPkt{cmd, data}
			<-done
		}
	}()

	//Keep all work in this thread
	go func() {
		for {
			select {
			case pkt := <-pktCh:
				s.handle(pkt.cmd, pkt.data)
				done <- 1
			case command := <-s.mercuryCommands:
				if command.commandType == SUBSCRIBE {
					s.mercury.Subscribe(command.uri, command.responseCh)
				} else {
					s.mercury.request(command.request, command.responseCh)
				}
			}
		}
	}()
}

func (s *Session) MercurySubscribe(uri string, responseCh chan MercuryResponse) {
	s.mercuryCommands <- command{
		commandType: SUBSCRIBE,
		uri:         uri,
		responseCh:  responseCh,
	}
}

func (s *Session) MercurySendRequest(request MercuryRequest, responseCh chan MercuryResponse) {
	s.mercuryCommands <- command{
		commandType: REQUEST,
		request:     request,
	}
}

func (s *Session) handle(cmd uint8, data []byte) {
	switch {
	case cmd == 0x4:
		err := s.stream.SendPacket(0x49, data)
		if err != nil {
			log.Fatal(err)
		}
	case cmd == 0x1b:
	case 0xb2 < cmd && cmd < 0xb6:
		err := s.mercury.handle(cmd, bytes.NewReader(data))
		if err != nil {
			log.Fatal(err)
		}
	case cmd == 0xac:
		welcome := &Spotify.APWelcome{}
		err := proto.Unmarshal(data, welcome)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("Authentication succeedded: ", welcome.GetCanonicalUsername())

	case cmd == 0xad:
		fmt.Println("Authentication failed")
	default:
	}
}

func (s *Session) Poll() {
	cmd, data, err := s.stream.RecvPacket()
	if err != nil {
		log.Fatal(err)
	}
	s.handle(cmd, data)
}

func getLoginBlobPacket(appfile string, blob blobInfo) []byte {
	data, _ := base64.StdEncoding.DecodeString(blob.DecodedBlob)

	buffer := bytes.NewBuffer(data)
	buffer.ReadByte()
	readBytes(buffer)
	buffer.ReadByte()
	authNum := readInt(buffer)
	authType := Spotify.AuthenticationType(authNum)
	buffer.ReadByte()
	authData := readBytes(buffer)
	
	return loginPacket(appfile, blob.Username, authData, &authType)
}

func readInt(b *bytes.Buffer) uint32{
	c, _ := b.ReadByte()
	lo := uint32(c)
	if lo & 0x80 == 0 {
		return lo
	}

	c2, _ := b.ReadByte()
	hi := uint32(c2)
	return lo & 0x7f | hi << 7
}

func readBytes(b *bytes.Buffer) []byte {
	length := readInt(b)
	data := make([]byte, length)
	b.Read(data)

	return data
}

func loginPacketPassword(appfile string, username string, password string) []byte{
	return loginPacket(appfile, username, []byte(password), Spotify.AuthenticationType_AUTHENTICATION_USER_PASS.Enum())
}

func loginPacket(appfile string, username string, authData []byte, authType *Spotify.AuthenticationType) []byte {
	data, err := ioutil.ReadFile(appfile)
	if err != nil {
		log.Fatal("failed to open spotify appkey file")
	}
	packet := &Spotify.ClientResponseEncrypted{
		LoginCredentials: &Spotify.LoginCredentials{
			Username: proto.String(username),
			Typ:      authType,
			AuthData: authData,
		},
		SystemInfo: &Spotify.SystemInfo{
			CpuFamily: Spotify.CpuFamily_CPU_UNKNOWN.Enum(),
			Os:        Spotify.Os_OS_UNKNOWN.Enum(),
			SystemInformationString: proto.String("librespot"),
			DeviceId:                proto.String("7288edd0fc3ffcbe93a0cf06e3568e28521687bc"),
		},
		VersionString: proto.String("librespot-8315e10"),
		Appkey: &Spotify.LibspotifyAppKey{
			Version:      proto.Uint32(uint32(data[0])),
			Devkey:       data[0x1:0x81],
			Signature:    data[0x81:0x141],
			Useragent:    proto.String("librespot-8315e10"),
			CallbackHash: make([]byte, 20),
		},
	}

	packetData, err := proto.Marshal(packet)
	if err != nil {
		log.Fatal("login marshaling error: ", err)
	}
	return packetData
}

func helloPacket(publicKey []byte) []byte {
	hello := &Spotify.ClientHello{
		BuildInfo: &Spotify.BuildInfo{
			Product:  Spotify.Product_PRODUCT_LIBSPOTIFY_EMBEDDED.Enum(),
			Platform: Spotify.Platform_PLATFORM_LINUX_X86.Enum(),
			Version:  proto.Uint64(0x10800000000),
		},
		CryptosuitesSupported: []Spotify.Cryptosuite{
			Spotify.Cryptosuite_CRYPTO_SUITE_SHANNON},
		LoginCryptoHello: &Spotify.LoginCryptoHelloUnion{
			DiffieHellman: &Spotify.LoginCryptoDiffieHellmanHello{
				Gc:              publicKey,
				ServerKeysKnown: proto.Uint32(1),
			},
		},
		ClientNonce: randomVec(0x10),
		FeatureSet: &Spotify.FeatureSet{
			Autoupdate2: proto.Bool(true),
		},
	}

	packetData, err := proto.Marshal(hello)
	if err != nil {
		log.Fatal("login marshaling error: ", err)
	}

	return packetData
}
