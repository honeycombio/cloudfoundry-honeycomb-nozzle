package main

// cache is an in-memory cache to store AppID <-> AppName mappings for data
// augmentation

import (
	"fmt"
	"sync"

	cfClient "github.com/cloudfoundry-community/go-cfclient"
)

type inMemCache struct {
	gcfClient *cfClient.Client
	contents  map[string]string
	lock      sync.Mutex
}

func (c *inMemCache) getNameFromID(appGuid string) (string, error) {
	if c.contents == nil {
		c.contents = make(map[string]string)
	}
	if c.gcfClient == nil {
		return "", fmt.Errorf("must initialize cfClient before use")
	}
	if appName, ok := c.contents[appGuid]; ok {
		return appName, nil
	}
	app, err := c.gcfClient.AppByGuid(appGuid)
	if err != nil {
		return "", fmt.Errorf("failed to retrieve name: %s", err)
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	c.contents[appGuid] = app.Name
	return app.Name, nil
}
