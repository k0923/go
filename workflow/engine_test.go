package workflow

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testEngine(t *testing.T, store Infrastructure, options Options) *Engine {
	t.Helper()
	e,err:=NewEngine(store,options)
	if err!=nil { t.Fatal(err) }
	return e
}

func definition[I,O any](fn FlowFunc[I,O]) Workflow[I,O] {
	return Workflow[I,O]{Name:"test",Version:"1",Flow:fn}
}

func TestNewEngine(t *testing.T) {
	for _,option:=range []Options{{LeaseTTL:-1},{CleanupTimeout:-1},{LeaseTTL:1}} {
		if _,err:=NewEngine(NewMemoryInfrastructure(),option);!errors.Is(err,ErrInvalid) { t.Fatalf("invalid options: %v",err) }
	}
	if _,err:=NewEngine(nil,Options{});!errors.Is(err,ErrInvalid) { t.Fatal(err) }
}

func TestEngineDrive(t *testing.T) {
	t.Run("replay and explicit input",func(t *testing.T) {
		store:=NewMemoryInfrastructure()
		e:=testEngine(t,store,Options{})
		calls:=0
		var phasesMu sync.Mutex
		var phases []string
		e.options.Observer=func(_ context.Context,o Observation) {
			phasesMu.Lock(); defer phasesMu.Unlock(); phases=append(phases,o.Phase)
		}
		flow:=definition(FlowFunc[int,int](func(f *FlowContext,in int) (int,error) {
			return f.Run("double",func(ctx context.Context,in int) (int,error) {
				calls++
				if ActionKey(ctx)!="double" { t.Fatalf("key: %s",ActionKey(ctx)) }
				if ctx.Value("trace")!="parent" { t.Fatal("context value lost") }
				return in*2,nil
			},in)
		}))
		ctx:=context.WithValue(context.Background(),"trace","parent")
		for i:=0;i<2;i++ {
			result,err:=e.Drive(ctx,flow,3)
			if err!=nil || result.Status!=Completed || result.Output!=6 { t.Fatalf("%+v %v",result,err) }
		}
		if calls!=1 { t.Fatalf("calls=%d",calls) }
		found:=false
		for _,phase:=range phases { if phase=="action_replayed" { found=true } }
		if !found { t.Fatal("replay not observed") }
		_,err:=e.Drive(ctx,flow,4)
		if !errors.Is(err,ErrChanged) { t.Fatalf("input mismatch: %v",err) }
		flow.Version="2"
		_,err=e.Drive(ctx,flow,3)
		if !errors.Is(err,ErrChanged) { t.Fatalf("version mismatch: %v",err) }
	})
	t.Run("ordinary failure retries",func(t *testing.T) {
		store:=NewMemoryInfrastructure();e:=testEngine(t,store,Options{})
		transient:=errors.New("503")
		calls:=0
		flow:=definition(FlowFunc[int,int](func(f *FlowContext,in int) (int,error) {
			return f.Run("service",func(context.Context,int) (int,error) {
				calls++;if calls==1 { return 0,transient };return 9,nil
			},in)
		}))
		result,err:=e.Drive(context.Background(),flow,1)
		if !errors.Is(err,transient) || result.Status!=Running { t.Fatalf("%+v %v",result,err) }
		r,_:=store.Get(context.Background(),"service")
		if r.Status!=Running { t.Fatalf("permanently failed: %+v",r) }
		result,err=e.Drive(context.Background(),flow,1)
		if err!=nil || result.Output!=9 || calls!=2 { t.Fatalf("%+v %v calls=%d",result,err,calls) }
	})
	t.Run("permanent and replayed error",func(t *testing.T) {
		store:=NewMemoryInfrastructure();e:=testEngine(t,store,Options{})
		cause:=errors.New("declined")
		calls:=0
		flow:=definition(FlowFunc[int,int](func(f *FlowContext,in int) (int,error) {
			return f.Run("pay",func(context.Context,int) (int,error) { calls++;return 0,Permanent(cause) },in)
		}))
		result,err:=e.Drive(context.Background(),flow,1)
		if result.Status!=Failed || !errors.Is(err,cause) { t.Fatalf("%+v %v",result,err) }
		result,err=e.Drive(context.Background(),flow,1)
		var replay *ReplayError
		if result.Status!=Failed || !errors.As(err,&replay) || replay.Message!="declined" || calls!=1 { t.Fatalf("%+v %v",result,err) }
	})
	t.Run("panic retains stack and can retry",func(t *testing.T) {
		e:=testEngine(t,NewMemoryInfrastructure(),Options{})
		broken:=true
		flow:=definition(FlowFunc[int,int](func(f *FlowContext,in int) (int,error) {
			return f.Run("bug",func(context.Context,int) (int,error) {
				if broken { panic("broken") };return 7,nil
			},in)
		}))
		result,err:=e.Drive(context.Background(),flow,0)
		var p *PanicError
		if !errors.As(err,&p) || len(p.Stack)==0 || result.Status!=Running { t.Fatalf("%+v %v",result,err) }
		broken=false
		result,err=e.Drive(context.Background(),flow,0)
		if err!=nil || result.Output!=7 { t.Fatalf("%+v %v",result,err) }
	})
	t.Run("timeout leaves running and releases",func(t *testing.T) {
		store:=NewMemoryInfrastructure();e:=testEngine(t,store,Options{})
		flow:=definition(FlowFunc[int,int](func(f *FlowContext,in int) (int,error) {
			return f.Run("slow",func(ctx context.Context,_ int) (int,error) {
				<-ctx.Done();return 1,nil
			},in,ActionOptions{Timeout:time.Millisecond})
		}))
		result,err:=e.Drive(context.Background(),flow,0)
		if !errors.Is(err,context.DeadlineExceeded) || result.Status!=Running { t.Fatalf("%+v %v",result,err) }
		r,_:=store.Get(context.Background(),"slow")
		if r.Status!=Running { t.Fatal(r) }
		if _,err:=store.Claim(context.Background(),"slow",time.Second);err!=nil { t.Fatal(err) }
	})
	t.Run("invalid definition and cancelled input",func(t *testing.T) {
		e:=testEngine(t,NewMemoryInfrastructure(),Options{})
		_,err:=e.Drive(context.Background(),Workflow[int,int]{},0)
		if !errors.Is(err,ErrInvalid) { t.Fatal(err) }
		ctx,cancel:=context.WithCancel(context.Background());cancel()
		_,err=e.Drive(ctx,definition(FlowFunc[int,int](func(*FlowContext,int) (int,error) { t.Fatal("executed");return 0,nil })),0)
		if !errors.Is(err,context.Canceled) { t.Fatal(err) }
	})
	t.Run("ignored suspension never reports success",func(t *testing.T) {
		e:=testEngine(t,NewMemoryInfrastructure(),Options{})
		flow:=definition(FlowFunc[int,int](func(f *FlowContext,in int) (int,error) {
			_,_=f.WaitFor[string]("approval",in)
			return 123,nil
		}))
		result,err:=e.Drive(context.Background(),flow,0)
		if err!=nil || result.Status!=Suspended || result.Output!=0 || len(result.Pending)!=1 { t.Fatalf("%+v %v",result,err) }
	})
	t.Run("observer panic isolated",func(t *testing.T) {
		e:=testEngine(t,NewMemoryInfrastructure(),Options{Observer:func(context.Context,Observation){panic("metrics")}})
		result,err:=e.Drive(context.Background(),definition(FlowFunc[int,int](func(f *FlowContext,in int) (int,error) {
			return f.Run("pure",Pure(func(n int) int { return n+1 }),in)
		})),4)
		if err!=nil || result.Output!=5 { t.Fatalf("%+v %v",result,err) }
	})
}

func TestFlowContextRun(t *testing.T) {
	t.Run("duplicate and invalid keys",func(t *testing.T) {
		for _,key:=range []string{"","x|y","中文","a-b","one"} {
			e:=testEngine(t,NewMemoryInfrastructure(),Options{})
			flow:=definition(FlowFunc[int,int](func(f *FlowContext,in int) (int,error) {
				_,_=f.Run("one",Pure(func(n int) int {return n}),in)
				return f.Run(key,Pure(func(n int) int {return n}),in)
			}))
			_,err:=e.Drive(context.Background(),flow,0)
			if !errors.Is(err,ErrInvalid) { t.Fatalf("key=%q: %v",key,err) }
		}
	})
	t.Run("concurrent drivers execute once",func(t *testing.T) {
		store:=NewMemoryInfrastructure();e:=testEngine(t,store,Options{})
		started:=make(chan struct{});finish:=make(chan struct{})
		var calls atomic.Int32
		flow:=definition(FlowFunc[int,int](func(f *FlowContext,in int) (int,error) {
			return f.Run("exclusive",func(context.Context,int) (int,error) {
				calls.Add(1);close(started);<-finish;return 4,nil
			},in)
		}))
		first:=make(chan error,1)
		go func(){_,err:=e.Drive(context.Background(),flow,0);first<-err}()
		<-started
		_,err:=e.Drive(context.Background(),flow,0)
		if !errors.Is(err,ErrConflict) { t.Fatalf("second: %v",err) }
		close(finish)
		if err=<-first;err!=nil { t.Fatal(err) }
		if calls.Load()!=1 { t.Fatal(calls.Load()) }
	})
	t.Run("action input change rejected",func(t *testing.T) {
		e:=testEngine(t,NewMemoryInfrastructure(),Options{})
		input:=1
		flow:=definition(FlowFunc[int,int](func(f *FlowContext,_ int) (int,error) {
			return f.Run("step",Pure(func(n int) int{return n}),input)
		}))
		if _,err:=e.Drive(context.Background(),flow,0);err!=nil { t.Fatal(err) }
		input=2
		if _,err:=e.Drive(context.Background(),flow,0);!errors.Is(err,ErrChanged) { t.Fatal(err) }
	})
}

func TestFlowContextRunFlow(t *testing.T) {
	e:=testEngine(t,NewMemoryInfrastructure(),Options{})
	leaf:=FlowFunc[int,string](func(f *FlowContext,in int) (string,error) {
		return f.Run("action",func(ctx context.Context,_ int) (string,error) {return ActionKey(ctx),nil},in)
	})
	middle:=FlowFunc[int,string](func(f *FlowContext,in int) (string,error) {return f.RunFlow("leaf",leaf,in)})
	flow:=definition(FlowFunc[int,string](func(f *FlowContext,in int) (string,error) {return f.RunFlow("middle",middle,in)}))
	result,err:=e.Drive(context.Background(),flow,1)
	if err!=nil || result.Output!="middle|leaf|action" { t.Fatalf("%+v %v",result,err) }
}
