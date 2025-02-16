package rpc

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/edaniels/golog"
	"github.com/pion/ice/v2"
	"github.com/pion/interceptor"
	"github.com/pion/sctp"
	"github.com/pion/webrtc/v3"
	"go.uber.org/multierr"

	"go.viam.com/utils"
	webrtcpb "go.viam.com/utils/proto/rpc/webrtc/v1"
)

// DefaultICEServers is the default set of ICE servers to use for WebRTC session negotiation.
// There is no guarantee that the defaults here will remain usable.
var DefaultICEServers = []webrtc.ICEServer{
	// feel free to use your own ICE servers
	{
		URLs: []string{"stun:global.stun.twilio.com:3478"},
	},
}

// DefaultWebRTCConfiguration is the standard configuration used for WebRTC peers.
var DefaultWebRTCConfiguration = webrtc.Configuration{
	ICEServers: DefaultICEServers,
}

func newWebRTCAPI(isClient bool, logger golog.Logger) (*webrtc.API, error) {
	m := webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	i := interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(&m, &i); err != nil {
		return nil, err
	}

	var settingEngine webrtc.SettingEngine
	if isClient {
		settingEngine.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryAndGather)
	} else {
		settingEngine.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryOnly)
	}
	// by including the loopback candidate, we allow an offline mode such that the
	// server/client (controlled/controlling) can include 127.0.0.1 as a candidate
	// while the client (controlling) provides an mDNS candidate that may resolve to 127.0.0.1.
	settingEngine.SetIncludeLoopbackCandidate(true)
	settingEngine.SetRelayAcceptanceMinWait(3 * time.Second)
	settingEngine.SetIPFilter(func(ip net.IP) bool {
		// Disallow ipv6 addresses since grpc-go does not currently support IPv6 scoped literals.
		// See related grpc-go issue: https://github.com/grpc/grpc-go/issues/3272.
		//
		// Stolen from net/ip.go, `IP.String` method.
		if p4 := ip.To4(); len(p4) == net.IPv4len {
			return true
		}

		return false
	})

	options := []func(a *webrtc.API){webrtc.WithMediaEngine(&m), webrtc.WithInterceptorRegistry(&i)}
	if utils.Debug {
		settingEngine.LoggerFactory = WebRTCLoggerFactory{logger}
	}
	options = append(options, webrtc.WithSettingEngine(settingEngine))
	return webrtc.NewAPI(options...), nil
}

func newPeerConnectionForClient(
	ctx context.Context,
	config webrtc.Configuration,
	disableTrickle bool,
	logger golog.Logger,
) (*webrtc.PeerConnection, *webrtc.DataChannel, error) {
	webAPI, err := newWebRTCAPI(true, logger)
	if err != nil {
		return nil, nil, err
	}

	peerConn, err := webAPI.NewPeerConnection(config)
	if err != nil {
		return nil, nil, err
	}
	var successful bool
	defer func() {
		if !successful {
			err = multierr.Combine(err, peerConn.Close())
		}
	}()

	negotiated := true
	ordered := true
	dataChannelID := uint16(0)
	dataChannel, err := peerConn.CreateDataChannel("data", &webrtc.DataChannelInit{
		ID:         &dataChannelID,
		Negotiated: &negotiated,
		Ordered:    &ordered,
	})
	if err != nil {
		return peerConn, nil, err
	}
	dataChannel.OnError(initialDataChannelOnError(peerConn, logger))

	if disableTrickle {
		offer, err := peerConn.CreateOffer(nil)
		if err != nil {
			return peerConn, nil, err
		}

		// Sets the LocalDescription, and starts our UDP listeners
		err = peerConn.SetLocalDescription(offer)
		if err != nil {
			return peerConn, nil, err
		}

		// Create channel that is blocked until ICE Gathering is complete
		gatherComplete := webrtc.GatheringCompletePromise(peerConn)

		// Block until ICE Gathering is complete since we signal back one complete SDP
		// and do not want to wait on trickle ICE.
		select {
		case <-ctx.Done():
			return peerConn, nil, ctx.Err()
		case <-gatherComplete:
		}
	}

	// Will not wait for connection to establish. If you want this in the future,
	// add a state check to OnICEConnectionStateChange for webrtc.ICEConnectionStateConnected.
	successful = true
	return peerConn, dataChannel, nil
}

func newPeerConnectionForServer(
	ctx context.Context,
	sdp string,
	config webrtc.Configuration,
	disableTrickle bool,
	logger golog.Logger,
) (*webrtc.PeerConnection, *webrtc.DataChannel, error) {
	webAPI, err := newWebRTCAPI(false, logger)
	if err != nil {
		return nil, nil, err
	}

	peerConn, err := webAPI.NewPeerConnection(config)
	if err != nil {
		return nil, nil, err
	}
	var successful bool
	defer func() {
		if !successful {
			err = multierr.Combine(err, peerConn.Close())
		}
	}()

	var negOpen bool
	var negMu sync.Mutex
	var negotiationChannel *webrtc.DataChannel
	var makingOffer bool
	peerConn.OnNegotiationNeeded(func() {
		negMu.Lock()
		if !negOpen {
			negMu.Unlock()
			return
		}
		negMu.Unlock()
		makingOffer = true
		defer func() {
			makingOffer = false
		}()
		offer, err := peerConn.CreateOffer(nil)
		if err != nil {
			logger.Errorw("renegotiation: error creating offer", "error", err)
			return
		}
		if err := peerConn.SetLocalDescription(offer); err != nil {
			logger.Errorw("renegotiation: error setting local description", "error", err)
			return
		}
		encodedSDP, err := encodeSDP(peerConn.LocalDescription())
		if err != nil {
			logger.Errorw("renegotiation: error encoding SDP", "error", err)
			return
		}
		if err := negotiationChannel.SendText(encodedSDP); err != nil {
			logger.Errorw("renegotiation: error sending SDP", "error", err)
			return
		}
	})

	negotiated := true
	ordered := true
	dataChannelID := uint16(0)
	dataChannel, err := peerConn.CreateDataChannel("data", &webrtc.DataChannelInit{
		ID:         &dataChannelID,
		Negotiated: &negotiated,
		Ordered:    &ordered,
	})
	if err != nil {
		return peerConn, dataChannel, err
	}
	dataChannel.OnError(initialDataChannelOnError(peerConn, logger))

	negotiationChannelID := uint16(1)
	negotiationChannel, err = peerConn.CreateDataChannel("negotiation", &webrtc.DataChannelInit{
		ID:         &negotiationChannelID,
		Negotiated: &negotiated,
		Ordered:    &ordered,
	})
	if err != nil {
		return peerConn, dataChannel, err
	}
	negotiationChannel.OnError(initialDataChannelOnError(peerConn, logger))

	negotiationChannel.OnOpen(func() {
		negMu.Lock()
		negOpen = true
		negMu.Unlock()
	})

	const polite = false
	var ignoreOffer bool
	negotiationChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
		negMu.Lock()
		defer negMu.Unlock()

		description := webrtc.SessionDescription{}
		if err := decodeSDP(string(msg.Data), &description); err != nil {
			logger.Errorw("renegotiation: error decoding SDP", "error", err)
			return
		}
		offerCollision := (description.Type == webrtc.SDPTypeOffer) &&
			(makingOffer || peerConn.SignalingState() != webrtc.SignalingStateStable)
		ignoreOffer = !polite && offerCollision
		if ignoreOffer {
			logger.Debugw("ignoring offer", "polite", polite, "offer_collision", offerCollision)
		}

		if err := peerConn.SetRemoteDescription(description); err != nil {
			logger.Errorw("renegotiation: error setting remote description", "error", err)
			return
		}

		if description.Type == webrtc.SDPTypeOffer {
			answer, err := peerConn.CreateAnswer(nil)
			if err != nil {
				logger.Errorw("renegotiation: error creating answer", "error", err)
				return
			}
			if err := peerConn.SetLocalDescription(answer); err != nil {
				logger.Errorw("renegotiation: error setting local description", "error", err)
				return
			}
			encodedSDP, err := encodeSDP(peerConn.LocalDescription())
			if err != nil {
				logger.Errorw("renegotiation: error encoding SDP", "error", err)
				return
			}
			if err := negotiationChannel.SendText(encodedSDP); err != nil {
				logger.Errorw("renegotiation: error sending SDP", "error", err)
				return
			}
		}
	})

	offer := webrtc.SessionDescription{}
	if err := decodeSDP(sdp, &offer); err != nil {
		return peerConn, dataChannel, err
	}

	err = peerConn.SetRemoteDescription(offer)
	if err != nil {
		return peerConn, dataChannel, err
	}

	if disableTrickle {
		answer, err := peerConn.CreateAnswer(nil)
		if err != nil {
			return peerConn, dataChannel, err
		}

		err = peerConn.SetLocalDescription(answer)
		if err != nil {
			return peerConn, dataChannel, err
		}

		// Create channel that is blocked until ICE Gathering is complete
		gatherComplete := webrtc.GatheringCompletePromise(peerConn)

		// Block until ICE Gathering is complete since we signal back one complete SDP
		// and do not want to wait on trickle ICE.
		select {
		case <-ctx.Done():
			return peerConn, nil, ctx.Err()
		case <-gatherComplete:
		}
	}

	successful = true
	return peerConn, dataChannel, nil
}

type webrtcPeerConnectionStats struct {
	ID               string
	RemoteCandidates map[string]string
}

func webrtcPeerConnCandPair(peerConnection *webrtc.PeerConnection) (*webrtc.ICECandidatePair, bool) {
	connectionState := peerConnection.ICEConnectionState()
	if connectionState == webrtc.ICEConnectionStateConnected && peerConnection.SCTP() != nil &&
		peerConnection.SCTP().Transport() != nil &&
		peerConnection.SCTP().Transport().ICETransport() != nil {
		candPair, err := peerConnection.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
		if err != nil {
			return nil, false
		}
		return candPair, true
	}
	return nil, false
}

func getWebRTCPeerConnectionStats(peerConnection *webrtc.PeerConnection) webrtcPeerConnectionStats {
	stats := peerConnection.GetStats()
	var connID string
	connInfo := map[string]string{}
	for _, stat := range stats {
		if pcStats, ok := stat.(webrtc.PeerConnectionStats); ok {
			connID = pcStats.ID
		}
		candidateStats, ok := stat.(webrtc.ICECandidateStats)
		if !ok {
			continue
		}
		if candidateStats.Type != webrtc.StatsTypeRemoteCandidate {
			continue
		}
		var candidateType string
		switch candidateStats.CandidateType {
		case webrtc.ICECandidateTypeRelay:
			candidateType = "relay"
		case webrtc.ICECandidateTypePrflx:
			candidateType = "peer-reflexive"
		case webrtc.ICECandidateTypeSrflx:
			candidateType = "server-reflexive"
		case webrtc.ICECandidateTypeHost:
			candidateType = "host"
		}
		if candidateType == "" {
			continue
		}
		connInfo[candidateType] = candidateStats.IP
	}
	return webrtcPeerConnectionStats{connID, connInfo}
}

func initialDataChannelOnError(pc io.Closer, logger golog.Logger) func(err error) {
	return func(err error) {
		if errors.Is(err, sctp.ErrResetPacketInStateNotExist) ||
			isUserInitiatedAbortChunkErr(err) {
			return
		}
		logger.Errorw("premature data channel error before WebRTC channel association", "error", err)
		utils.UncheckedError(pc.Close())
	}
}

func iceCandidateToProto(i *webrtc.ICECandidate) *webrtcpb.ICECandidate {
	return iceCandidateInitToProto(i.ToJSON())
}

func iceCandidateInitToProto(ij webrtc.ICECandidateInit) *webrtcpb.ICECandidate {
	candidate := webrtcpb.ICECandidate{
		Candidate: ij.Candidate,
	}
	if ij.SDPMid != nil {
		val := *ij.SDPMid
		candidate.SdpMid = &val
	}
	if ij.SDPMLineIndex != nil {
		val := uint32(*ij.SDPMLineIndex)
		candidate.SdpmLineIndex = &val
	}
	if ij.UsernameFragment != nil {
		val := *ij.UsernameFragment
		candidate.UsernameFragment = &val
	}
	return &candidate
}

func iceCandidateFromProto(i *webrtcpb.ICECandidate) webrtc.ICECandidateInit {
	candidate := webrtc.ICECandidateInit{
		Candidate: i.Candidate,
	}
	if i.SdpMid != nil {
		val := *i.SdpMid
		candidate.SDPMid = &val
	}
	if i.SdpmLineIndex != nil {
		val := uint16(*i.SdpmLineIndex)
		candidate.SDPMLineIndex = &val
	}
	if i.UsernameFragment != nil {
		val := *i.UsernameFragment
		candidate.UsernameFragment = &val
	}
	return candidate
}
