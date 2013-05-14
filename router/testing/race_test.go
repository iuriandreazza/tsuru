// Copyright 2013 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build race

package testing

import (
	"fmt"
	"launchpad.net/gocheck"
	"runtime"
	"sync"
)

func (s *S) TestAddRouteAndRemoteRouteAreSafe(c *gocheck.C) {
	var wg sync.WaitGroup
	fake := FakeRouter{}
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	for i := 1; i < 256; i++ {
		wg.Add(3)
		name := fmt.Sprintf("route-%d", i)
		ip := fmt.Sprintf("10.10.10.%d", i)
		go func(i int) {
			fake.AddRoute(name, ip)
			wg.Done()
		}(i)
		go func() {
			fake.RemoveRoute(name)
			wg.Done()
		}()
		go func() {
			fake.HasRoute(name)
			wg.Done()
		}()
	}
	wg.Wait()
}