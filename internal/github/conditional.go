package github

import (
	"container/list"
	"net/url"
	"strings"
)

const (
	maxCachedPages = 1024
	maxCachedBytes = 16 << 20
)

type pageKey struct{ root, url string }

type cachedPage struct {
	key                  pageKey
	finalURL, etag, next string
	body                 []byte
}

type pageCache struct {
	pages       map[pageKey]*list.Element
	inventories map[string]map[string]bool
	lru         list.List
	bytes       int
}

// EnableConditionalReads keeps validators and bodies for this controller's credential.
// Its scoped clients share the cache and use it serially.
func (c *Client) EnableConditionalReads() {
	if c.token != "" {
		c.pages = &pageCache{pages: make(map[pageKey]*list.Element), inventories: make(map[string]map[string]bool)}
	}
}

func (c *pageCache) get(k pageKey) (cachedPage, bool) {
	el, ok := c.pages[k]
	if !ok {
		return cachedPage{}, false
	}
	c.lru.MoveToFront(el)
	return el.Value.(cachedPage), true
}

func (c *pageCache) save(page cachedPage) {
	c.remove(c.pages[page.key])
	if page.etag == "" || len(page.body) > maxCachedBytes {
		return
	}
	for len(c.pages) >= maxCachedPages || c.bytes+len(page.body) > maxCachedBytes {
		c.remove(c.lru.Back())
	}
	c.pages[page.key] = c.lru.PushFront(page)
	c.bytes += len(page.body)
}

func (c *pageCache) remove(el *list.Element) {
	if el != nil {
		page := el.Value.(cachedPage)
		delete(c.pages, page.key)
		c.bytes -= len(page.body)
		c.lru.Remove(el)
	}
}

func (c *pageCache) prune(root string, seen map[string]bool) {
	for k, el := range c.pages {
		if k.root == root && !seen[k.url] {
			c.remove(el)
		}
	}
}

func (c *pageCache) invalidate(raw string) {
	u, err := url.Parse(raw)
	if err == nil {
		if parts := strings.SplitN(u.Path, "/", 5); len(parts) >= 4 && parts[1] == "repos" {
			c.invalidateRepo(parts[2] + "/" + parts[3])
			return
		}
	}
	clear(c.pages)
	clear(c.inventories)
	c.lru.Init()
	c.bytes = 0
}

func (c *pageCache) invalidateRepo(repo string) {
	prefix := "/repos/" + strings.ToLower(repo) + "/"
	for k, el := range c.pages {
		u, err := url.Parse(k.root)
		if err == nil && strings.HasPrefix(strings.ToLower(u.Path), prefix) {
			c.remove(el)
		}
	}
}

func (c *pageCache) setInventory(key string, repos []Repo) {
	next := make(map[string]bool, len(repos))
	for _, repo := range repos {
		next[repo.Name] = repo.Private
	}
	for name, private := range c.inventories[key] {
		if listed, ok := next[name]; !ok || listed != private {
			c.invalidateRepo(name)
		}
	}
	c.inventories[key] = next
}
