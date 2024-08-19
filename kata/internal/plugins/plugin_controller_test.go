/*
 * Copyright © 2024 Kaleido, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on
 * an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 *
 * SPDX-License-Identifier: Apache-2.0
 */
package plugins

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/hyperledger/firefly-common/pkg/log"
	"github.com/kaleido-io/paladin/toolkit/pkg/confutil"
	"github.com/kaleido-io/paladin/toolkit/pkg/prototk"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type testPlugin interface {
	conf() *PluginConfig
	run(t *testing.T, ctx context.Context, id string, client prototk.PluginControllerClient)
}

type testPluginLoader struct {
	plugins map[string]testPlugin
	done    chan struct{}
}

func tempUDS(t *testing.T) string {
	// Not safe to use t.TempDir() as it generates too long paths including the test name
	f, err := os.CreateTemp("", "ut_*.sock")
	assert.NoError(t, err)
	_ = f.Close()
	allocatedUDSName := f.Name()
	err = os.Remove(allocatedUDSName)
	assert.NoError(t, err)
	t.Cleanup(func() {
		err := os.Remove(allocatedUDSName)
		assert.True(t, err == nil || os.IsNotExist(err))
	})
	return allocatedUDSName
}

func (tpl *testPluginLoader) run(t *testing.T, ctx context.Context, targetURL string, loaderID uuid.UUID) {
	wg := new(sync.WaitGroup)
	defer func() {
		wg.Wait()
		close(tpl.done)
	}()

	conn, err := grpc.NewClient(targetURL, grpc.WithTransportCredentials(insecure.NewCredentials()))
	assert.NoError(t, err)
	defer conn.Close() // will close all the child conns too

	client := prototk.NewPluginControllerClient(conn)

	loaderStream, err := client.InitLoader(ctx, &prototk.PluginLoaderInit{
		Id: loaderID.String(),
	})
	assert.NoError(t, err)

	for {
		msg, err := loaderStream.Recv()
		if err != nil {
			log.L(ctx).Infof("loader stream closed: %s", err)
			return
		}
		tp := tpl.plugins[msg.Plugin.Name]
		if tp != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tp.run(t, ctx, msg.Plugin.Id, client)
			}()
		}
	}

}

func newTestDomainPluginController(t *testing.T, tdm *testDomainManager, testDomains map[string]*testDomain) (context.Context, *pluginController, func()) {
	ctx, cancelCtx := context.WithCancel(context.Background())

	args := &PluginControllerArgs{
		DomainManager: tdm,
		LoaderID:      uuid.New(),
		InitialConfig: &PluginControllerConfig{
			GRPC: GRPCConfig{
				Address:         tempUDS(t),
				ShutdownTimeout: confutil.P("1ms"),
			},
			Domains: make(map[string]*PluginConfig),
		},
	}
	testPlugins := make(map[string]testPlugin)
	for name, td := range testDomains {
		args.InitialConfig.Domains[name] = td.conf()
		testPlugins[name] = td
	}

	pc, err := NewPluginController(ctx, args)
	assert.NoError(t, err)

	tpl := &testPluginLoader{
		plugins: testPlugins,
		done:    make(chan struct{}),
	}
	err = pc.Start(ctx)
	assert.NoError(t, err)

	go tpl.run(t, ctx, pc.GRPCTargetURL(), pc.LoaderID())

	return ctx, pc.(*pluginController), func() {
		recovered := recover()
		if recovered != nil {
			fmt.Fprintf(os.Stderr, "%v: %s", recovered, debug.Stack())
			panic(recovered)
		}
		cancelCtx()
		pc.Stop(ctx)
		<-tpl.done
	}

}

func TestInitPluginControllerBadPlugin(t *testing.T) {
	args := &PluginControllerArgs{
		DomainManager: nil,
		LoaderID:      uuid.New(),
		InitialConfig: &PluginControllerConfig{
			GRPC: GRPCConfig{Address: tempUDS(t)},
			Domains: map[string]*PluginConfig{
				"!badname": {},
			},
		},
	}
	_, err := NewPluginController(context.Background(), args)
	assert.Regexp(t, "PD011106", err)
}

func TestInitPluginControllerBadSocket(t *testing.T) {
	args := &PluginControllerArgs{
		DomainManager: nil,
		LoaderID:      uuid.New(),
		InitialConfig: &PluginControllerConfig{
			GRPC: GRPCConfig{Address: t.TempDir() /* can't use a dir as a socket */},
		},
	}
	pc, err := NewPluginController(context.Background(), args)
	assert.NoError(t, err)

	err = pc.Start(context.Background())
	assert.Regexp(t, "bind", err)
}

func TestInitPluginControllerUDSTooLong(t *testing.T) {
	longerThanUDSSafelySupportsCrossPlatform := make([]rune, 187)
	for i := 0; i < len(longerThanUDSSafelySupportsCrossPlatform); i++ {
		longerThanUDSSafelySupportsCrossPlatform[i] = (rune)('a' + (i % 26))
	}

	args := &PluginControllerArgs{
		DomainManager: nil,
		LoaderID:      uuid.New(),
		InitialConfig: &PluginControllerConfig{
			GRPC: GRPCConfig{Address: string(longerThanUDSSafelySupportsCrossPlatform)},
		},
	}
	_, err := NewPluginController(context.Background(), args)
	assert.Regexp(t, "PD011205", err)
}

func TestInitPluginControllerTCP4(t *testing.T) {
	longerThanUDSSafelySupportsCrossPlatform := make([]rune, 187)
	for i := 0; i < len(longerThanUDSSafelySupportsCrossPlatform); i++ {
		longerThanUDSSafelySupportsCrossPlatform[i] = (rune)('a' + (i % 26))
	}

	args := &PluginControllerArgs{
		DomainManager: nil,
		LoaderID:      uuid.New(),
		InitialConfig: &PluginControllerConfig{
			GRPC: GRPCConfig{Address: "tcp4:0.0.0.0:0"},
		},
	}
	pc, err := NewPluginController(context.Background(), args)
	assert.NoError(t, err)

	err = pc.Start(context.Background())
	assert.NoError(t, err)
	assert.True(t, strings.HasPrefix(pc.GRPCTargetURL(), "dns:///"))
}

func TestInitPluginControllerTCP6(t *testing.T) {
	longerThanUDSSafelySupportsCrossPlatform := make([]rune, 187)
	for i := 0; i < len(longerThanUDSSafelySupportsCrossPlatform); i++ {
		longerThanUDSSafelySupportsCrossPlatform[i] = (rune)('a' + (i % 26))
	}

	args := &PluginControllerArgs{
		DomainManager: nil,
		LoaderID:      uuid.New(),
		InitialConfig: &PluginControllerConfig{
			GRPC: GRPCConfig{Address: "tcp6:[::1]:0"},
		},
	}
	pc, err := NewPluginController(context.Background(), args)
	assert.NoError(t, err)

	err = pc.Start(context.Background())
	assert.NoError(t, err)
	assert.True(t, strings.HasPrefix(pc.GRPCTargetURL(), "dns:///"))
}

func TestNotifyPluginUpdateNotStarted(t *testing.T) {
	args := &PluginControllerArgs{
		DomainManager: nil,
		LoaderID:      uuid.New(),
		InitialConfig: &PluginControllerConfig{
			GRPC: GRPCConfig{Address: tempUDS(t)},
		},
	}
	pc, err := NewPluginController(context.Background(), args)
	assert.NoError(t, err)

	err = pc.WaitForInit(context.Background())
	assert.NoError(t, err)

	err = pc.PluginsUpdated(&PluginControllerConfig{})
	assert.NoError(t, err)
	err = pc.PluginsUpdated(&PluginControllerConfig{})
	assert.NoError(t, err)
}

func TestLoaderErrors(t *testing.T) {
	ctx := context.Background()
	args := &PluginControllerArgs{
		DomainManager: nil,
		LoaderID:      uuid.New(),
		InitialConfig: &PluginControllerConfig{
			GRPC: GRPCConfig{
				Address:         "tcp:127.0.0.1:0",
				ShutdownTimeout: confutil.P("1ms"),
			},
			Domains: map[string]*PluginConfig{
				"domain1": {
					Type:     LibraryTypeJar.Enum(),
					Location: "some/where",
				},
			},
		},
	}
	pc, err := NewPluginController(ctx, args)
	assert.NoError(t, err)

	err = pc.Start(ctx)
	assert.NoError(t, err)

	conn, err := grpc.NewClient(pc.GRPCTargetURL(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	assert.NoError(t, err)
	defer conn.Close() // will close all the child conns too

	client := prototk.NewPluginControllerClient(conn)

	// first load with wrong ID
	wrongLoader, err := client.InitLoader(ctx, &prototk.PluginLoaderInit{
		Id: uuid.NewString(),
	})
	assert.NoError(t, err)
	_, err = wrongLoader.Recv()
	assert.Regexp(t, "PD011200", err)

	// then load correctly
	loaderStream, err := client.InitLoader(ctx, &prototk.PluginLoaderInit{
		Id: pc.LoaderID().String(),
	})
	assert.NoError(t, err)

	loadReq, err := loaderStream.Recv()
	assert.NoError(t, err)

	_, err = client.LoadFailed(ctx, &prototk.PluginLoadFailed{
		Plugin:       loadReq.Plugin,
		ErrorMessage: "pop",
	})
	assert.NoError(t, err)

	// We should be notified of the error if we were waiting
	err = pc.WaitForInit(ctx)
	assert.Regexp(t, "pop", err)

	// then attempt double start of the loader
	dupLoader, err := client.InitLoader(ctx, &prototk.PluginLoaderInit{
		Id: pc.LoaderID().String(),
	})
	assert.NoError(t, err)
	_, err = dupLoader.Recv()
	assert.Regexp(t, "PD011201", err)

	// If we come back, we won't be (only one caller of WaitForInit supported)
	// - check it times out context not an error on load
	cancelled, cancelCtx := context.WithCancel(context.Background())
	cancelCtx()
	err = pc.WaitForInit(cancelled)
	assert.Regexp(t, "PD010301", err)

	err = loaderStream.CloseSend()
	assert.NoError(t, err)

	// Notify of a plugin after closed stream
	err = pc.PluginsUpdated(&PluginControllerConfig{
		Domains: map[string]*PluginConfig{
			"domain2": {
				Type:     LibraryTypeCShared.Enum(),
				Location: "some/where/else",
			},
		},
	})
	assert.NoError(t, err)

	pc.Stop(ctx)

	// Also check we don't block on the LoadFailed notification if the channel gets full (which it will after stop)
	for i := 0; i < 3; i++ {
		_, _ = pc.(*pluginController).LoadFailed(context.Background(), &prototk.PluginLoadFailed{Plugin: &prototk.PluginInfo{}})
	}
}
