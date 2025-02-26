// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package jaegerreceiver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/apache/thrift/lib/go/thrift"
	"github.com/jaegertracing/jaeger/cmd/agent/app/servers/thriftudp"
	"github.com/jaegertracing/jaeger/model"
	jaegerconvert "github.com/jaegertracing/jaeger/model/converter/thrift/jaeger"
	"github.com/jaegertracing/jaeger/proto-gen/api_v2"
	"github.com/jaegertracing/jaeger/thrift-gen/agent"
	jaegerthrift "github.com/jaegertracing/jaeger/thrift-gen/jaeger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/config"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/model/pdata"
	conventions "go.opentelemetry.io/collector/model/semconv/v1.5.0"
	"google.golang.org/grpc"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/coreinternal/testutil"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/translator/jaeger"
)

var jaegerAgent = config.NewIDWithName(typeStr, "agent_test")

func TestJaegerAgentUDP_ThriftCompact(t *testing.T) {
	port := testutil.GetAvailablePort(t)
	addrForClient := fmt.Sprintf(":%d", port)
	testJaegerAgent(t, addrForClient, &configuration{
		AgentCompactThriftPort:   int(port),
		AgentCompactThriftConfig: DefaultServerConfigUDP(),
	})
}

func TestJaegerAgentUDP_ThriftCompact_InvalidPort(t *testing.T) {
	port := 999999

	config := &configuration{
		AgentCompactThriftPort: port,
	}
	set := componenttest.NewNopReceiverCreateSettings()
	jr := newJaegerReceiver(jaegerAgent, config, nil, set)

	assert.Error(t, jr.Start(context.Background(), componenttest.NewNopHost()), "should not have been able to startTraceReception")

	require.NoError(t, jr.Shutdown(context.Background()))
}

func TestJaegerAgentUDP_ThriftBinary(t *testing.T) {
	port := testutil.GetAvailablePort(t)
	addrForClient := fmt.Sprintf(":%d", port)
	testJaegerAgent(t, addrForClient, &configuration{
		AgentBinaryThriftPort:   int(port),
		AgentBinaryThriftConfig: DefaultServerConfigUDP(),
	})
}

func TestJaegerAgentUDP_ThriftBinary_PortInUse(t *testing.T) {
	// This test confirms that the thrift binary port is opened correctly.  This is all we can test at the moment.  See above.
	port := testutil.GetAvailablePort(t)

	config := &configuration{
		AgentBinaryThriftPort:   int(port),
		AgentBinaryThriftConfig: DefaultServerConfigUDP(),
	}
	set := componenttest.NewNopReceiverCreateSettings()
	jr := newJaegerReceiver(jaegerAgent, config, nil, set)

	assert.NoError(t, jr.startAgent(componenttest.NewNopHost()), "Start failed")
	t.Cleanup(func() { require.NoError(t, jr.Shutdown(context.Background())) })

	l, err := net.Listen("udp", fmt.Sprintf("localhost:%d", port))
	assert.Error(t, err, "should not have been able to listen to the port")

	if l != nil {
		l.Close()
	}
}

func TestJaegerAgentUDP_ThriftBinary_InvalidPort(t *testing.T) {
	port := 999999

	config := &configuration{
		AgentBinaryThriftPort: port,
	}
	set := componenttest.NewNopReceiverCreateSettings()
	jr := newJaegerReceiver(jaegerAgent, config, nil, set)

	assert.Error(t, jr.Start(context.Background(), componenttest.NewNopHost()), "should not have been able to startTraceReception")

	require.NoError(t, jr.Shutdown(context.Background()))
}

func initializeGRPCTestServer(t *testing.T, beforeServe func(server *grpc.Server), opts ...grpc.ServerOption) (*grpc.Server, net.Addr) {
	server := grpc.NewServer(opts...)
	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	beforeServe(server)
	go func() {
		err := server.Serve(lis)
		require.NoError(t, err)
	}()
	return server, lis.Addr()
}

type mockSamplingHandler struct {
}

func (*mockSamplingHandler) GetSamplingStrategy(context.Context, *api_v2.SamplingStrategyParameters) (*api_v2.SamplingStrategyResponse, error) {
	return &api_v2.SamplingStrategyResponse{StrategyType: api_v2.SamplingStrategyType_PROBABILISTIC}, nil
}

func TestJaegerHTTP(t *testing.T) {
	s, addr := initializeGRPCTestServer(t, func(s *grpc.Server) {
		api_v2.RegisterSamplingManagerServer(s, &mockSamplingHandler{})
	})
	defer s.GracefulStop()

	port := testutil.GetAvailablePort(t)
	config := &configuration{
		AgentHTTPPort: int(port),
		RemoteSamplingClientSettings: configgrpc.GRPCClientSettings{
			Endpoint: addr.String(),
			TLSSetting: &configtls.TLSClientSetting{
				Insecure: true,
			},
		},
	}
	set := componenttest.NewNopReceiverCreateSettings()
	jr := newJaegerReceiver(jaegerAgent, config, nil, set)
	t.Cleanup(func() { require.NoError(t, jr.Shutdown(context.Background())) })

	assert.NoError(t, jr.Start(context.Background(), componenttest.NewNopHost()), "Start failed")

	// allow http server to start
	assert.Eventually(t, func() bool {
		conn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", port))
		if err == nil && conn != nil {
			conn.Close()
			return true
		}
		return false
	}, 10*time.Second, 5*time.Millisecond, "failed to wait for the port to be open")

	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/sampling?service=test", port))
	assert.NoError(t, err, "should not have failed to make request")
	if resp != nil {
		assert.Equal(t, 200, resp.StatusCode, "should have returned 200")
	}

	resp, err = http.Get(fmt.Sprintf("http://localhost:%d/sampling?service=test", port))
	assert.NoError(t, err, "should not have failed to make request")
	if resp != nil {
		assert.Equal(t, 200, resp.StatusCode, "should have returned 200")
	}

	resp, err = http.Get(fmt.Sprintf("http://localhost:%d/baggageRestrictions?service=test", port))
	assert.NoError(t, err, "should not have failed to make request")
	if resp != nil {
		assert.Equal(t, 200, resp.StatusCode, "should have returned 200")
	}
}

func testJaegerAgent(t *testing.T, agentEndpoint string, receiverConfig *configuration) {
	// 1. Create the Jaeger receiver aka "server"
	sink := new(consumertest.TracesSink)
	set := componenttest.NewNopReceiverCreateSettings()
	jr := newJaegerReceiver(jaegerAgent, receiverConfig, sink, set)
	t.Cleanup(func() { require.NoError(t, jr.Shutdown(context.Background())) })

	assert.NoError(t, jr.Start(context.Background(), componenttest.NewNopHost()), "Start failed")

	// 2. Then send spans to the Jaeger receiver.
	jexp, err := newClientUDP(agentEndpoint, jr.agentBinaryThriftEnabled())
	assert.NoError(t, err, "Failed to create the Jaeger OpenCensus exporter for the live application")

	// 3. Now finally send some spans
	td := generateTraceData()
	batches, err := jaeger.InternalTracesToJaegerProto(td)
	require.NoError(t, err)
	for _, batch := range batches {
		require.NoError(t, jexp.EmitBatch(context.Background(), modelToThrift(batch)))
	}

	assert.Eventually(t, func() bool {
		return sink.SpanCount() > 0
	}, 10*time.Second, 5*time.Millisecond)

	gotTraces := sink.AllTraces()
	require.Equal(t, 1, len(gotTraces))
	assert.EqualValues(t, td, gotTraces[0])
}

func newClientUDP(hostPort string, binary bool) (*agent.AgentClient, error) {
	clientTransport, err := thriftudp.NewTUDPClientTransport(hostPort, "")
	if err != nil {
		return nil, err
	}
	var protocolFactory thrift.TProtocolFactory
	if binary {
		protocolFactory = thrift.NewTBinaryProtocolFactoryConf(nil)
	} else {
		protocolFactory = thrift.NewTCompactProtocolFactoryConf(nil)
	}
	return agent.NewAgentClientFactory(clientTransport, protocolFactory), nil
}

// Cannot use the testdata because timestamps are nanoseconds.
func generateTraceData() pdata.Traces {
	td := pdata.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().UpsertString(conventions.AttributeServiceName, "test")
	span := rs.InstrumentationLibrarySpans().AppendEmpty().Spans().AppendEmpty()
	span.SetSpanID(pdata.NewSpanID([8]byte{0, 1, 2, 3, 4, 5, 6, 7}))
	span.SetTraceID(pdata.NewTraceID([16]byte{0, 1, 2, 3, 4, 5, 6, 7, 7, 6, 5, 4, 3, 2, 1, 0}))
	span.SetStartTimestamp(1581452772000000000)
	span.SetEndTimestamp(1581452773000000000)
	return td
}

func modelToThrift(batch *model.Batch) *jaegerthrift.Batch {
	return &jaegerthrift.Batch{
		Process: processModelToThrift(batch.Process),
		Spans:   jaegerconvert.FromDomain(batch.Spans),
	}
}

func processModelToThrift(process *model.Process) *jaegerthrift.Process {
	if process == nil {
		return nil
	}
	return &jaegerthrift.Process{
		ServiceName: process.ServiceName,
	}
}
