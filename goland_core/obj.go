package core

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"syscall"

	bpf "github.com/aquasecurity/libbpfgo"
	"golang.org/x/sys/unix"
)

const (
	RL_CPU_ANY = 1 << 20
)

type Sched struct {
	mod        *bpf.Module
	bss        *BssMap
	uei        *UeiMap
	structOps  *bpf.BPFMap
	queue      chan []byte // The map containing tasks that are queued to user space from the kernel.
	dispatch   chan []byte
	exitEvt    chan []byte
	selectCpu  *bpf.BPFProg
	siblingCpu *bpf.BPFProg
	urb        *bpf.UserRingBuffer
	erb        *bpf.RingBuffer
}

func init() {
	unix.Mlockall(syscall.MCL_CURRENT | syscall.MCL_FUTURE)
}

func LoadSched(objPath string) *Sched {
	obj := LoadSkel()
	bpfModule, err := bpf.NewModuleFromFileArgs(bpf.NewModuleArgs{
		BPFObjPath:     "",
		KernelLogLevel: 0,
	})
	if err != nil {
		panic(err)
	}
	if err := bpfModule.BPFLoadExistedObject(obj); err != nil {
		panic(err)
	}

	s := &Sched{
		mod: bpfModule,
	}
	iters := bpfModule.Iterator()
	for {
		prog := iters.NextProgram()
		if prog == nil {
			break
		}
		if prog.Name() == "kprobe_handle_mm_fault" {
			log.Println("attach kprobe_handle_mm_fault")
			_, err := prog.AttachGeneric()
			if err != nil {
				log.Panicf("attach kprobe_handle_mm_fault failed: %v", err)
			}
			continue
		}
		if prog.Name() == "kretprobe_handle_mm_fault" {
			log.Println("attach kretprobe_handle_mm_fault")
			_, err := prog.AttachGeneric()
			if err != nil {
				log.Panicf("attach kretprobe_handle_mm_fault failed: %v", err)
			}
			continue
		}
	}
	iters = bpfModule.Iterator()
	for {
		m := iters.NextMap()
		if m == nil {
			break
		}
		fmt.Printf("map: %s, type: %s, fd: %d\n", m.Name(), m.Type().String(), m.FileDescriptor())
		if m.Name() == "main_bpf.bss" {
			s.bss = &BssMap{m}
		} else if m.Name() == "main_bpf.data" {
			s.uei = &UeiMap{m}
		} else if m.Name() == "queued" {
			s.queue = make(chan []byte, 4096)
			rb, err := s.mod.InitRingBuf("queued", s.queue)
			if err != nil {
				panic(err)
			}
			rb.Poll(50)
		} else if m.Name() == "dispatched" {
			s.dispatch = make(chan []byte, 4096)
			s.urb, err = s.mod.InitUserRingBuf("dispatched", s.dispatch)
			if err != nil {
				panic(err)
			}
			s.urb.Start()
		} else if m.Name() == "exit_rb" {
			s.exitEvt = make(chan []byte, 256)
			s.erb, err = s.mod.InitRingBuf("exit_rb", s.exitEvt)
			if err != nil {
				panic(err)
			}
			s.erb.Poll(300)
		}
		if m.Type().String() == "BPF_MAP_TYPE_STRUCT_OPS" {
			s.structOps = m
		}
	}

	iters = bpfModule.Iterator()
	for {
		prog := iters.NextProgram()
		if prog == nil {
			break
		}

		if prog.Name() == "rs_select_cpu" {
			s.selectCpu = prog
		}

		if prog.Name() == "enable_sibling_cpu" {
			s.siblingCpu = prog
		}
	}

	return s
}

type task_cpu_arg struct {
	pid   int32
	cpu   int32
	flags uint64
}

var selectFailed error = fmt.Errorf("prog (selectCpu) not found")

func (s *Sched) SelectCPU(t *QueuedTask) (error, int32) {
	if s.selectCpu != nil {
		arg := &task_cpu_arg{
			pid:   t.Pid,
			cpu:   t.Cpu,
			flags: t.Flags,
		}
		var data bytes.Buffer
		binary.Write(&data, binary.LittleEndian, arg)
		opt := bpf.RunOpts{
			CtxIn:     data.Bytes(),
			CtxSizeIn: uint32(data.Len()),
		}
		err := s.selectCpu.Run(&opt)
		if err != nil {
			return err, 0
		}
		if opt.RetVal > 2147483647 {
			return nil, RL_CPU_ANY
		}
		return nil, int32(opt.RetVal)
	}
	return selectFailed, 0
}

type domain_arg struct {
	lvlId        int32
	cpuId        int32
	siblingCpuId int32
}

func (s *Sched) EnableSiblingCpu(lvlId, cpuId, siblingCpuId int32) error {
	if s.siblingCpu != nil {
		arg := &domain_arg{
			lvlId:        lvlId,
			cpuId:        cpuId,
			siblingCpuId: siblingCpuId,
		}
		var data bytes.Buffer
		binary.Write(&data, binary.LittleEndian, arg)
		opt := bpf.RunOpts{
			CtxIn:     data.Bytes(),
			CtxSizeIn: uint32(data.Len()),
		}
		err := s.siblingCpu.Run(&opt)
		if err != nil {
			return err
		}
		if opt.RetVal != 0 {
			return fmt.Errorf("retVal: %v", opt.RetVal)
		}
		return nil
	}
	return fmt.Errorf("prog (siblingCpu) not found")
}

func (s *Sched) Attach() error {
	_, err := s.structOps.AttachStructOps()
	return err
}

func (s *Sched) Close() {
	s.erb.Close()
	s.urb.Close()
	s.mod.Close()
}
