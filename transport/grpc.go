// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package transport

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"time"

	"github.com/opentracing/opentracing-go"
	"go.uber.org/multierr"
	apipeer "go.uber.org/yarpc/api/peer"
	"go.uber.org/yarpc/api/transport"
	"go.uber.org/yarpc/peer"
	"go.uber.org/yarpc/peer/hostport"
	"go.uber.org/yarpc/peer/roundrobin"
	"go.uber.org/yarpc/transport/grpc"
	"golang.org/x/net/context"
)

var (
	errGRPCNoAddresses = errors.New("must specify at least one grpc address")
	errGRPCNoTracer    = errors.New("must specify grpc tracer")
	errGRPCNoCaller    = errors.New("must specify grpc caller")
	errGRPCNoService   = errors.New("must specify grpc service")
	errGRPCNoProcedure = errors.New("must specify grpc procedure")
)

// GRPCOptions are used to create a GRPC transport.
type GRPCOptions struct {
	Addresses       []string
	Tracer          opentracing.Tracer
	Caller          string
	Encoding        string
	RoutingKey      string
	RoutingDelegate string
	MaxResponseSize int
	CAPath          string
	CertPath        string
	PrivateKeyPath  string
}

// NewGRPC returns a transport that calls a GRPC service.
func NewGRPC(options GRPCOptions) (TransportCloser, error) {
	return newGRPC(options)
}

type grpcTransport struct {
	Transport       transport.Transport
	Outbound        transport.UnaryOutbound
	StreamOutbound  transport.StreamOutbound
	Caller          string
	Encoding        string
	RoutingKey      string
	RoutingDelegate string
	tracer          opentracing.Tracer
}

func newGRPC(options GRPCOptions) (*grpcTransport, error) {
	if len(options.Addresses) == 0 {
		return nil, errGRPCNoAddresses
	}
	if options.Tracer == nil {
		return nil, errGRPCNoTracer
	}
	if options.Caller == "" {
		return nil, errGRPCNoCaller
	}

	transportOptions := []grpc.TransportOption{grpc.Tracer(options.Tracer)}
	if options.MaxResponseSize > 0 {
		transportOptions = append(transportOptions, grpc.ClientMaxRecvMsgSize(options.MaxResponseSize))
	}

	transport := grpc.NewTransport(transportOptions...)
	var peerTransport apipeer.Transport = transport
	if options.CAPath != "" && options.CertPath != "" && options.PrivateKeyPath != "" {
		ca, err := ioutil.ReadFile(options.CAPath)
		if err != nil {
			return nil, fmt.Errorf("could not load ca %v", err)
		}

		certPool := x509.NewCertPool()
		if !certPool.AppendCertsFromPEM(ca) {
			return nil, errors.New("failed to append ca")
		}

		clientCert, err := tls.LoadX509KeyPair(options.CertPath, options.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load X509 keypair %v", err)
		}

		dialOptions := []grpc.DialOption{
			grpc.DialerTLSConfig(&tls.Config{
				RootCAs:            certPool,
				Certificates:       []tls.Certificate{clientCert},
				InsecureSkipVerify: true,
			}),
		}

		peerTransport = transport.NewDialer(dialOptions...)
	}
	outbound := transport.NewOutbound(peer.Bind(roundrobin.New(peerTransport), peer.BindPeers(peersToIdentifiers(options.Addresses))))

	if err := transport.Start(); err != nil {
		return nil, err
	}
	if err := outbound.Start(); err != nil {
		_ = transport.Stop()
		return nil, err
	}

	return &grpcTransport{
		Transport:       transport,
		Outbound:        outbound,
		StreamOutbound:  outbound,
		Caller:          options.Caller,
		Encoding:        options.Encoding,
		RoutingKey:      options.RoutingKey,
		RoutingDelegate: options.RoutingDelegate,
		tracer:          options.Tracer,
	}, nil
}

func (t *grpcTransport) Tracer() opentracing.Tracer {
	return t.tracer
}

func (t *grpcTransport) Protocol() Protocol {
	return GRPC
}

func (t *grpcTransport) Call(ctx context.Context, request *Request) (*Response, error) {
	if request.TargetService == "" {
		return nil, errGRPCNoService
	}
	if request.Method == "" {
		return nil, errGRPCNoProcedure
	}

	ctx, cancel := requestContextWithTimeout(ctx, request)
	defer cancel()
	transportResponse, err := t.Outbound.Call(ctx, t.requestToYARPCRequest(request))
	if err != nil {
		return nil, err
	}
	return yarpcResponseToResponse(transportResponse)
}

func (t *grpcTransport) CallStream(ctx context.Context, request *StreamRequest) (*transport.ClientStream, error) {
	return t.StreamOutbound.CallStream(ctx, t.requestToYARPCStreamRequest(request))
}

func (t *grpcTransport) Close() error {
	return multierr.Combine(t.Transport.Stop(), t.Outbound.Stop())
}

func (t *grpcTransport) requestToYARPCStreamRequest(streamRequest *StreamRequest) *transport.StreamRequest {
	if t == nil || streamRequest == nil || streamRequest.Request == nil {
		return nil
	}

	return &transport.StreamRequest{
		Meta: &transport.RequestMeta{
			Caller:          t.Caller,
			Service:         streamRequest.Request.TargetService,
			Encoding:        transport.Encoding(t.Encoding),
			Procedure:       streamRequest.Request.Method,
			Headers:         transport.HeadersFromMap(streamRequest.Request.Headers),
			ShardKey:        streamRequest.Request.ShardKey,
			RoutingKey:      t.RoutingKey,
			RoutingDelegate: t.RoutingDelegate,
		},
	}
}

func (t *grpcTransport) requestToYARPCRequest(request *Request) *transport.Request {
	return &transport.Request{
		Caller:          t.Caller,
		Service:         request.TargetService,
		Encoding:        transport.Encoding(t.Encoding),
		Procedure:       request.Method,
		Headers:         transport.HeadersFromMap(request.Headers),
		ShardKey:        request.ShardKey,
		RoutingKey:      t.RoutingKey,
		RoutingDelegate: t.RoutingDelegate,
		Body:            bytes.NewReader(request.Body),
	}
}

func requestContextWithTimeout(ctx context.Context, request *Request) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	timeout := time.Second
	if request.Timeout > 0 {
		timeout = request.Timeout
	}
	return context.WithTimeout(ctx, timeout)
}

func yarpcResponseToResponse(transportResponse *transport.Response) (*Response, error) {
	response := &Response{
		Headers: transportResponse.Headers.Items(),
	}
	if transportResponse.Body != nil {
		body, err := ioutil.ReadAll(transportResponse.Body)
		if err != nil {
			return nil, err
		}
		if err := transportResponse.Body.Close(); err != nil {
			return nil, err
		}
		response.Body = body
	}
	return response, nil
}

func peersToIdentifiers(peers []string) []apipeer.Identifier {
	identifiers := make([]apipeer.Identifier, len(peers))
	for i, peer := range peers {
		identifiers[i] = hostport.Identify(peer)
	}
	return identifiers
}
