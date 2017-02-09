package consul

import (
	"fmt"
	"io/ioutil"
	mathrand "math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/consul/testutil"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
)

func skipChaos(t *testing.T) {
	if os.Getenv("NOMAD_CHAOS") == "" {
		t.Skip("Set NOMAD_CHAOS to run slow consul tests")
	}
}

// TestSyncerTaskFlapping tests for service registrations flapping in Consul
// when two tasks are running: one whose name starts with the other's:
//
//	foo and foobar
//
// See https://github.com/hashicorp/nomad/issues/2294
func TestSyncerTaskFlapping(t *testing.T) {
	skipChaos(t)

	// Create an embedded Consul server
	testconsul := testutil.NewTestServerConfig(t, func(c *testutil.TestServerConfig) {
		// If -v wasn't specified squelch consul logging
		if !testing.Verbose() {
			c.Stdout = ioutil.Discard
			c.Stderr = ioutil.Discard
		}
	})
	defer testconsul.Stop()

	// Create two Syncers - one for each task
	cconf := config.DefaultConsulConfig()
	cconf.Addr = testconsul.HTTPAddr

	t1syncer, err := NewSyncer(cconf, nil, logger)
	if err != nil {
		t.Fatalf("Error creating Syncer: %v", err)
	}
	defer t1syncer.Shutdown()

	t2syncer, err := NewSyncer(cconf, nil, logger)
	if err != nil {
		t.Fatalf("Error creating Syncer: %v", err)
	}
	defer t2syncer.Shutdown()

	allocID := "1234" // must share the same alloc ID
	s1 := &structs.Service{Name: "foo"}
	s2 := &structs.Service{Name: "foobar"}

	s1domain := NewExecutorDomain(allocID, s1.Name)
	s2domain := NewExecutorDomain(allocID, s2.Name)

	s1services := map[ServiceKey]*structs.Service{
		GenerateServiceKey(s1): s1,
	}
	s2services := map[ServiceKey]*structs.Service{
		GenerateServiceKey(s2): s2,
	}

	// Add the services to the syncers
	if err := t1syncer.SetServices(s1domain, s1services); err != nil {
		t.Fatalf("error setting services: %v", err)
	}
	if err := t2syncer.SetServices(s2domain, s2services); err != nil {
		t.Fatalf("error setting services: %v", err)
	}

	// Run the syncers
	go t1syncer.Run()
	defer t1syncer.Shutdown()
	go t2syncer.Run()
	defer t2syncer.Shutdown()

	// Give them plenty of time to sync
	time.Sleep(3 * time.Second)

	// Watch for consistency violations (both services should always exist)
	dice := mathrand.New(mathrand.NewSource(time.Now().Unix()))
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {

		services, err := t1syncer.client.Agent().Services()
		if err != nil {
			t.Fatalf("%d - error querying consul: %v", i, err)
		}
		// 2 services + consul itself
		if expected := 3; len(services) != expected {
			t.Fatalf("%d - consistency violation; expected %d services but found %d: %#v", i, expected, len(services), services)
		}
		time.Sleep(time.Duration(dice.Intn(500)) * time.Millisecond)
	}
}

func TestSyncerChaos(t *testing.T) {
	skipChaos(t)

	// Create an embedded Consul server
	testconsul := testutil.NewTestServerConfig(t, func(c *testutil.TestServerConfig) {
		// If -v wasn't specified squelch consul logging
		if !testing.Verbose() {
			c.Stdout = ioutil.Discard
			c.Stderr = ioutil.Discard
		}
	})
	defer testconsul.Stop()

	// Configure Syncer to talk to the test server
	cconf := config.DefaultConsulConfig()
	cconf.Addr = testconsul.HTTPAddr

	clientSyncer, err := NewSyncer(cconf, nil, logger)
	if err != nil {
		t.Fatalf("Error creating Syncer: %v", err)
	}
	defer clientSyncer.Shutdown()

	execSyncer, err := NewSyncer(cconf, nil, logger)
	if err != nil {
		t.Fatalf("Error creating Syncer: %v", err)
	}
	defer execSyncer.Shutdown()

	clientService := &structs.Service{Name: "nomad-client"}
	services := map[ServiceKey]*structs.Service{
		GenerateServiceKey(clientService): clientService,
	}
	if err := clientSyncer.SetServices("client", services); err != nil {
		t.Fatalf("error setting client service: %v", err)
	}

	const execn = 100
	const reapern = 2
	errors := make(chan error, 100)
	wg := sync.WaitGroup{}

	// Start goroutines to concurrently SetServices
	for i := 0; i < execn; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			domain := ServiceDomain(fmt.Sprintf("exec-%d", i))
			services := map[ServiceKey]*structs.Service{}
			for ii := 0; ii < 10; ii++ {
				s := &structs.Service{Name: fmt.Sprintf("exec-%d-%d", i, ii)}
				services[GenerateServiceKey(s)] = s
				if err := execSyncer.SetServices(domain, services); err != nil {
					select {
					case errors <- err:
					default:
					}
					return
				}
				time.Sleep(1)
			}
		}(i)
	}

	// SyncServices runs a timer started by Syncer.Run which we don't use
	// in this test, so run SyncServices concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < execn; i++ {
			if err := execSyncer.SyncServices(); err != nil {
				select {
				case errors <- err:
				default:
				}
				return
			}
			time.Sleep(100)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := clientSyncer.ReapUnmatched([]ServiceDomain{"nomad-client"}); err != nil {
			select {
			case errors <- err:
			default:
			}
			return
		}
	}()

	// Reap all but exec-0-*
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < execn; i++ {
			if err := execSyncer.ReapUnmatched([]ServiceDomain{"exec-0", ServiceDomain(fmt.Sprintf("exec-%d", i))}); err != nil {
				select {
				case errors <- err:
				default:
				}
			}
			time.Sleep(100)
		}
	}()

	go func() {
		wg.Wait()
		close(errors)
	}()

	for err := range errors {
		if err != nil {
			t.Errorf("error setting service from executor goroutine: %v", err)
		}
	}

	// Do a final ReapUnmatched to get consul back into a deterministic state
	if err := execSyncer.ReapUnmatched([]ServiceDomain{"exec-0"}); err != nil {
		t.Fatalf("error doing final reap: %v", err)
	}

	// flattenedServices should be fully populated as ReapUnmatched doesn't
	// touch Syncer's internal state
	expected := map[string]struct{}{}
	for i := 0; i < execn; i++ {
		for ii := 0; ii < 10; ii++ {
			expected[fmt.Sprintf("exec-%d-%d", i, ii)] = struct{}{}
		}
	}

	for _, s := range execSyncer.flattenedServices() {
		_, ok := expected[s.Name]
		if !ok {
			t.Errorf("%s unexpected", s.Name)
		}
		delete(expected, s.Name)
	}
	if len(expected) > 0 {
		left := []string{}
		for s := range expected {
			left = append(left, s)
		}
		sort.Strings(left)
		t.Errorf("Couldn't find %d names in flattened services:\n%s", len(expected), strings.Join(left, "\n"))
	}

	// All but exec-0 and possibly some of exec-99 should have been reaped
	{
		services, err := execSyncer.client.Agent().Services()
		if err != nil {
			t.Fatalf("Error getting services: %v", err)
		}
		expected := []int{}
		for k, service := range services {
			if service.Service == "consul" {
				continue
			}
			i := -1
			ii := -1
			fmt.Sscanf(service.Service, "exec-%d-%d", &i, &ii)
			switch {
			case i == -1 || ii == -1:
				t.Errorf("invalid service: %s -> %s", k, service.Service)
			case i != 0 || ii > 9:
				t.Errorf("unexpected service: %s -> %s", k, service.Service)
			default:
				expected = append(expected, ii)
			}
		}
		if len(expected) != 10 {
			t.Errorf("expected 0-9 but found: %#q", expected)
		}
	}
}
