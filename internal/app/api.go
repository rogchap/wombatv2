package app

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/mitchellh/mapstructure"
	"github.com/wailsapp/wails"
	"github.com/wailsapp/wails/lib/logger"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

const defaultWorkspaceKey = "wksp_default"

type api struct {
	runtime       *wails.Runtime
	logger        *logger.CustomLogger
	client        *client
	store         *store
	protofiles    *protoregistry.Files
	cancelCtxFunc context.CancelFunc
}

// WailsInit is the init fuction for the wails runtime
func (a *api) WailsInit(runtime *wails.Runtime) error {
	a.runtime = runtime
	a.logger = runtime.Log.New("API")

	// TODO get app data file path per os
	dbPath := filepath.Join(".", ".data")

	var err error
	a.store, err = newStore(dbPath)
	if err != nil {
		return fmt.Errorf("app: failed to create database: %v", err)
	}

	a.runtime.Events.On("wails:loaded", a.wailsLoaded)

	return nil
}

func (a *api) wailsLoaded(data ...interface{}) {
	opts, err := a.GetWorkspaceOptions()
	if err != nil {
		a.logger.Errorf("%v", err)
		return
	}
	if err := a.Connect(opts); err != nil {
		a.logger.Errorf("%v", err)
	}
}

// WailsShutdown is the shutdown function that is called when wails shuts down
func (a *api) WailsShutdown() {
	a.store.close()
	if a.cancelCtxFunc != nil {
		a.cancelCtxFunc()
	}
	if a.client != nil {
		a.client.close()
	}
}

// GetWorkspaceOptions gets the workspace options from the store
func (a *api) GetWorkspaceOptions() (*options, error) {
	val, err := a.store.get([]byte(defaultWorkspaceKey))
	if err != nil {
		return nil, err
	}

	var opts *options
	dec := gob.NewDecoder(bytes.NewBuffer(val))
	err = dec.Decode(&opts)

	return opts, err
}

// Connect will attempt to connect a grpc server and parse any proto files
func (a *api) Connect(data interface{}) error {
	var opts options
	if err := mapstructure.Decode(data, &opts); err != nil {
		return err
	}

	if a.client != nil {
		if err := a.client.close(); err != nil {
			return fmt.Errorf("app: failed to close previous connection: %v", err)
		}
	}

	if a.cancelCtxFunc != nil {
		a.cancelCtxFunc()
	}

	a.client = &client{}
	if err := a.client.connect(opts); err != nil {
		return fmt.Errorf("app: failed to connect to server: %v", err)
	}

	a.runtime.Events.Emit(eventClientConnected, opts.Addr)

	ctx := context.Background()
	ctx, a.cancelCtxFunc = context.WithCancel(ctx)
	go a.monitorStateChanges(ctx)

	go a.loadProtoFiles(opts)
	go a.setWorkspaceOptions(opts)

	return nil
}

func (a *api) loadProtoFiles(opts options) {
	a.runtime.Events.Emit(eventServicesSelectChanged)

	var err error
	if opts.Reflect {
		if a.client == nil {
			a.logger.Error("unable to load proto files via reflection: client is <nil>")
		}
		if a.protofiles, err = protoFilesFromReflectionAPI(a.client.conn, nil); err != nil {
			//TODO Emit error to frontend
			a.logger.Errorf("error getting proto files from reflection API: %v", err)
		}
	}
	if !opts.Reflect {
		// TODO: load protos from disk
	}

	a.emitServicesSelect()
}

func (a *api) emitServicesSelect() {
	if a.protofiles == nil {
		return
	}

	var ss servicesSelect
	a.protofiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		sds := fd.Services()
		for i := 0; i < sds.Len(); i++ {
			var s serviceSelect
			sd := sds.Get(i)
			s.FullName = string(sd.FullName())

			mds := sd.Methods()
			for j := 0; j < mds.Len(); j++ {
				md := mds.Get(j)
				fname := fmt.Sprintf("/%s/%s", sd.FullName(), md.Name())
				s.Methods = append(s.Methods, methodSelect{
					Name:     string(md.Name()),
					FullName: fname,
				})
			}
			sort.SliceStable(s.Methods, func(i, j int) bool {
				return s.Methods[i].Name < s.Methods[j].Name
			})
			ss = append(ss, s)
		}
		return true
	})

	if len(ss) == 0 {
		return
	}

	sort.SliceStable(ss, func(i, j int) bool {
		return ss[i].FullName < ss[j].FullName
	})

	a.runtime.Events.Emit(eventServicesSelectChanged, ss)
}

func (a *api) setWorkspaceOptions(opts options) {
	var val bytes.Buffer
	enc := gob.NewEncoder(&val)
	enc.Encode(opts)
	a.store.set([]byte(defaultWorkspaceKey), val.Bytes())
}

func (a *api) monitorStateChanges(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			// this will panic if we are waiting for a state change and the client (and it's connection)
			// get GC'd without this context being canceled
			a.logger.Errorf("panic monitoring state changes: %v", r)
		}
	}()
	for {
		if a.client == nil || a.client.conn == nil {
			continue
		}
		state := a.client.conn.GetState()
		a.runtime.Events.Emit(eventClientStateChanged, state.String())
		if ok := a.client.conn.WaitForStateChange(ctx, state); !ok {
			a.logger.Debug("ending monitoring of state changes")
			return
		}
	}
}