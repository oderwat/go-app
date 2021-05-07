package app

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/maxence-charriere/go-app/v9/pkg/errors"
)

const (
	eventBufferSize  = 4096
	updateBufferSize = 64
	deferBufferSize  = 64
)

type engine struct {
	// The rate where component updates are performed (per seconds).
	UpdateRate int

	// The page.
	Page Page

	// Reports whether the engine runs in a server.
	RunsInServer bool

	// The storage use as local storage.
	LocalStorage BrowserStorage

	// The storage used as session storage.
	SessionStorage BrowserStorage

	// The function used to resolve static resource paths.
	ResolveStaticResources func(string) string

	// The body of the page.
	Body HTMLBody

	initOnce  sync.Once
	startOnce sync.Once
	closeOnce sync.Once
	wait      sync.WaitGroup
	msgMutex  sync.Mutex

	isMountedOnce bool
	events        chan event
	updates       map[Composer]struct{}
	updateQueue   []updateDescriptor
	defers        []event
	messages      map[string]map[string]msgHandler
}

func (e *engine) Dispatch(src UI, fn func(Context)) {
	if src == nil {
		src = e.Body
	}

	if src.Mounted() {
		e.events <- event{
			source:   src,
			function: fn,
		}
	}
}

func (e *engine) Defer(src UI, fn func(Context)) {
	if src == nil {
		src = e.Body
	}

	if src.Mounted() {
		e.events <- event{
			source:    src,
			deferable: true,
			function:  fn,
		}
	}
}

func (e *engine) Emit(src UI, fn func()) {
	if !src.Mounted() {
		return
	}

	if fn != nil {
		fn()
	}

	compoCount := 0
	for n := src; n != nil; n = n.parent() {
		compo, ok := n.(Composer)
		if !ok {
			continue
		}

		compoCount++
		if compoCount > 1 {
			e.Dispatch(compo, nil)
		}
	}
}

func (e *engine) Handle(msg string, src UI, h MsgHandler) {
	e.msgMutex.Lock()
	defer e.msgMutex.Unlock()

	key := fmt.Sprintf("%p-%p", src, h)

	handlers, ok := e.messages[msg]
	if !ok {
		handlers = make(map[string]msgHandler)
		e.messages[msg] = handlers
	}

	handlers[key] = msgHandler{
		src:      src,
		function: h,
	}
}

func (e *engine) Post(msg string, v interface{}) {
	e.msgMutex.Lock()
	defer e.msgMutex.Unlock()

	handlers, ok := e.messages[msg]
	if !ok {
		return
	}

	for k, h := range handlers {
		src := h.src
		if !src.Mounted() {
			delete(handlers, k)
		}

		function := h.function
		e.Dispatch(src, func(ctx Context) {
			function(ctx, v)
		})
	}
}

func (e *engine) Async(fn func()) {
	e.wait.Add(1)
	go func() {
		fn()
		e.wait.Done()
	}()
}

func (e *engine) Wait() {
	e.wait.Wait()
}

func (e *engine) Context() Context {
	return makeContext(e.Body)
}

func (e *engine) Consume() {
	for {
		select {
		case ev := <-e.events:
			if ev.deferable {
				e.defers = append(e.defers, ev)
			} else {
				e.execEvent(ev)
				e.scheduleComponentUpdate(ev.source)
			}

		default:
			e.updateComponents()
			e.execDeferableEvents()
			return
		}
	}
}

func (e *engine) ConsumeNext() {
	select {
	case ev := <-e.events:
		if ev.deferable {
			e.defers = append(e.defers, ev)
		} else {
			e.execEvent(ev)
			e.scheduleComponentUpdate(ev.source)
		}
		e.updateComponents()
		e.execDeferableEvents()

	default:
	}
}

func (e *engine) Close() {
	e.closeOnce.Do(func() {
		e.Consume()
		e.Wait()
		e.closeMessageHandlers()

		dismount(e.Body)
		e.Body = nil
		close(e.events)
	})
}

func (e *engine) PreRender() {
	e.Dispatch(e.Body, func(Context) {
		e.Body.preRender(e.Page)
	})
}

func (e *engine) Mount(n UI) {
	e.Dispatch(e.Body, func(Context) {
		if !e.isMountedOnce {
			if err := e.Body.(elemWithChildren).replaceChildAt(0, n); err != nil {
				panic(errors.New("mounting ui element failed").
					Tag("events-count", len(e.events)).
					Tag("events-capacity", cap(e.events)).
					Tag("updates-count", len(e.updates)).
					Tag("updates-queue-len", len(e.updateQueue)).
					Wrap(err))
			}

			e.isMountedOnce = true
			return
		}

		err := update(e.Body.children()[0], n)
		if err == nil {
			return
		}
		if !isErrReplace(err) {
			panic(errors.New("mounting ui element failed").
				Tag("events-count", len(e.events)).
				Tag("events-capacity", cap(e.events)).
				Tag("updates-count", len(e.updates)).
				Tag("updates-queue-len", len(e.updateQueue)).
				Wrap(err))
		}

		if err := e.Body.(elemWithChildren).replaceChildAt(0, n); err != nil {
			panic(errors.New("mounting ui element failed").
				Tag("events-count", len(e.events)).
				Tag("events-capacity", cap(e.events)).
				Tag("updates-count", len(e.updates)).
				Tag("updates-queue-len", len(e.updateQueue)).
				Wrap(err))
		}
	})
}

func (e *engine) Nav(u *url.URL) {
	if p, ok := e.Page.(*requestPage); ok {
		p.ReplaceURL(u)
	}

	e.Dispatch(e.Body, func(Context) {
		e.Body.onNav(u)
	})
}

func (e *engine) AppUpdate() {
	e.Dispatch(e.Body, func(Context) {
		e.Body.onAppUpdate()
	})
}

func (e *engine) AppResize() {
	e.Dispatch(e.Body, func(Context) {
		e.Body.onResize()
	})
}

func (e *engine) init() {
	e.initOnce.Do(func() {
		e.events = make(chan event, eventBufferSize)
		e.updates = make(map[Composer]struct{})
		e.updateQueue = make([]updateDescriptor, 0, updateBufferSize)
		e.defers = make([]event, 0, deferBufferSize)
		e.messages = make(map[string]map[string]msgHandler)

		if e.UpdateRate <= 0 {
			e.UpdateRate = 60
		}

		if e.Page == nil {
			u, _ := url.Parse("https://test.go-app.dev")
			e.Page = &requestPage{url: u}
		}

		if e.LocalStorage == nil {
			e.LocalStorage = newMemoryStorage()
		}

		if e.SessionStorage == nil {
			e.SessionStorage = newMemoryStorage()
		}

		if e.ResolveStaticResources == nil {
			e.ResolveStaticResources = func(path string) string {
				return path
			}
		}

		if e.Body == nil {
			body := Body().Body(Div())
			if err := mount(e, body); err != nil {
				panic(errors.New("mounting engine default body failed").Wrap(err))
			}
			e.Body = body
		}
	})
}

func (e *engine) start(ctx context.Context) {
	e.startOnce.Do(func() {
		updateInterval := time.Second / time.Duration(e.UpdateRate)
		currentInterval := time.Duration(updateInterval)

		updates := time.NewTicker(currentInterval)
		defer updates.Stop()

		cleanMessages := time.NewTicker(time.Minute)
		defer cleanMessages.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case ev := <-e.events:
				if currentInterval != updateInterval {
					currentInterval = updateInterval
					updates.Reset(currentInterval)
				}

				if ev.deferable {
					e.defers = append(e.defers, ev)
				} else {
					e.execEvent(ev)
					e.scheduleComponentUpdate(ev.source)
				}

			case <-updates.C:
				e.updateComponents()
				e.execDeferableEvents()

				if len(e.events) == 0 {
					currentInterval = time.Hour
					updates.Reset(currentInterval)
				}

			case <-cleanMessages.C:
				e.closeMessageHandlers()
			}
		}
	})
}

func (e *engine) execEvent(ev event) {
	if ev.source.Mounted() && ev.function != nil {
		ev.function(makeContext(ev.source))
	}
}

func (e *engine) scheduleComponentUpdate(n UI) {
	if !n.Mounted() {
		return
	}

	var compo Composer
	var depth int

	for {
		if c, isCompo := n.(Composer); compo == nil && isCompo {
			if _, isScheduled := e.updates[c]; isScheduled {
				return
			}
			compo = c
		}

		parent := n.parent()
		if parent == nil {
			break
		}

		if compo != nil {
			depth++
		}
		n = parent
	}

	if compo == nil {
		return
	}

	e.updates[compo] = struct{}{}
	e.updateQueue = append(e.updateQueue, updateDescriptor{
		compo:    compo,
		priority: depth + 1,
	})
}

func (e *engine) updateComponents() {
	if len(e.updates) == 0 {
		return
	}

	sortUpdateDescriptors(e.updateQueue)
	for _, ud := range e.updateQueue {
		compo := ud.compo
		if !compo.Mounted() {
			continue
		}

		if _, isNotUpdated := e.updates[compo]; !isNotUpdated {
			continue
		}

		if err := compo.updateRoot(); err != nil {
			panic(err)
		}
		e.componentUpdated(compo)
	}

	e.updateQueue = e.updateQueue[:0]
}

func (e *engine) componentUpdated(c Composer) {
	delete(e.updates, c)
}

func (e *engine) execDeferableEvents() {
	for _, ev := range e.defers {
		if ev.source.Mounted() {
			ev.function(makeContext(ev.source))
		}
	}
	e.defers = e.defers[:0]
}

func (e *engine) closeMessageHandlers() {
	e.msgMutex.Lock()
	defer e.msgMutex.Unlock()

	for _, handlers := range e.messages {
		for k, h := range handlers {
			if !h.src.Mounted() {
				delete(handlers, k)
			}
		}
	}
}

func (e *engine) currentPage() Page {
	return e.Page
}

func (e *engine) localStorage() BrowserStorage {
	return e.LocalStorage
}

func (e *engine) sessionStorage() BrowserStorage {
	return e.SessionStorage
}

func (e *engine) runsInServer() bool {
	return e.RunsInServer
}

func (e *engine) resolveStaticResource(path string) string {
	return e.ResolveStaticResources(path)
}

type event struct {
	source    UI
	deferable bool
	function  func(Context)
}

type updateDescriptor struct {
	compo    Composer
	priority int
}

func sortUpdateDescriptors(d []updateDescriptor) {
	sort.Slice(d, func(a, b int) bool {
		return d[a].priority < d[b].priority
	})
}

type msgHandler struct {
	src      UI
	function MsgHandler
}