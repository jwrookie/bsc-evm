package miner

import (
	"encoding/json"
	"math/big"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/valyala/fasthttp"
)

const (
	ROB_INTERVAL    time.Duration = 6 * time.Second
	REFETCH_TIME    time.Duration = 10 * time.Second
	MAX_TODOS       int           = 1024
	MAX_RESULT_CHAN int           = 200
	CHAIN_ID        int64         = 1
	DOMAIN          string        = "localhost"
	PORT            string        = "32080"
	TASK_URL        string        = "/internal/tasks"
)

const (
	WAITING TodoStatus = iota // 0
	SUCCESS                   // 1
	FAILED                    // 2
)

type (
	TodoStatus int
	TaskList   []*Task
)

type GetBotTasksRsp struct {
	Length int     `json:"length"`
	List   []*Task `json:"list"`
}

type GetBotTasksResponse struct {
	Code int            `json:"code"` // 0:成功 1：失败
	Msg  string         `json:"msg"`
	Data GetBotTasksRsp `json:"data"`
}

type SendResultRsp struct{}

type SendResultResponse struct {
	Code int           `json:"code"` // 0:成功 1：失败
	Msg  string        `json:"msg"`
	Data SendResultRsp `json:"data"`
}

type Result struct {
	ID        uint64     `json:"id"`
	PreTxHash string     `json:"pre_tx_hash"` // 成功也有可能是空，因为待查交易可能已经在之前的区块打包进去了
	Status    TodoStatus `json:"status"`
	UsedGas   uint64     `json:"used_gas"`
	Block     string     `json:"block"`
}

type Task struct {
	ID         uint64     `json:"id"`
	From       string     `json:"from"`
	To         string     `json:"to"`
	Data       []byte     `json:"data"`
	GasPrice   string     `json:"gas_price"`
	Value      string     `json:"value"`
	ResultWant bool       `json:"result_want"`
	Status     TodoStatus `json:"status"`
}

func (t *Task) Msg(env *environment) *core.Message {
	var to common.Address = common.HexToAddress(t.To)
	value := common.Big0
	value, _ = value.SetString(t.Value, 10)
	return &core.Message{
		GasLimit:          env.header.GasLimit,
		GasPrice:          common.Big0,
		GasFeeCap:         common.Big0,
		GasTipCap:         common.Big0,
		To:                &to,
		From:              common.HexToAddress(t.From),
		Value:             value,
		Data:              t.Data,
		SkipAccountChecks: true,
	}
}

type Robber struct {
	// todo_txs []*types.Transaction
	todos   TaskList
	running chan bool

	result      chan *Result
	should_try  atomic.Bool
	worker      *worker
	todoLock    sync.RWMutex
	http_client *fasthttp.Client
}

func newRobber(worker *worker) *Robber {
	rob := &Robber{
		worker:      worker,
		running:     make(chan bool),
		result:      make(chan *Result, MAX_RESULT_CHAN),
		http_client: &fasthttp.Client{},
	}

	go rob.mainloop()
	return rob
}

func (r *Robber) Start() {
	r.running <- true
	log.Info("rob start")
}

func (r *Robber) Stop() {
	close(r.running)
	log.Info("rob stop")
}

func (r *Robber) SendResult() {
	for {
		res := <-r.result
		log.Info("todo result", "result", res)
		url := strings.Join([]string{"http://", DOMAIN, ":", PORT, TASK_URL}, "")
		req := fasthttp.AcquireRequest()
		req.SetRequestURI(url)
		req.Header.SetMethod("POST")
		body, err := json.Marshal(res)
		if err != nil {
			log.Info(err.Error())
		}
		req.SetBody(body)
		resp := fasthttp.AcquireResponse()
		err = r.http_client.Do(req, resp)
		if err != nil {
			log.Info(err.Error())
		}
		fasthttp.ReleaseResponse(resp)
	}
}

func (r *Robber) mainloop() {
	fetch_timer := time.NewTimer(REFETCH_TIME)
	defer fetch_timer.Stop()
	<-r.running
	go r.SendResult()
	for {
		select {
		case <-fetch_timer.C:
			if err := r.AppendTodos(); err == nil {
				fetch_timer.Reset(REFETCH_TIME)
			}
		case running := <-r.running:
			if running {
				r.should_try.Store(false)
				t := time.NewTimer(ROB_INTERVAL)
				<-t.C
				r.should_try.Store(true)
			} else {
				r.should_try.Store(false)
				break
			}
		}
	}
}

func (r *Robber) try(work *environment) {
	if !r.should_try.Load() {
		return
	}
	r.todoLock.Lock()
	defer r.todoLock.Unlock()

	for _, todo := range r.todos {
		if todo.Status != WAITING {
			continue
		}
		wc := work.copy()
		result, err := r.tryOne(wc, todo)
		if err != nil {
			// 说明出了大问题，直接标记失败
			todo.Status = FAILED
			r.result <- &Result{
				ID:     todo.ID,
				Status: todo.Status,
			}
		} else if !result.Failed() == todo.ResultWant {
			// 成功了，要打包bundle了
			idx := sort.Search(len(wc.snapState), func(i int) bool {
				env := wc.copy()
				env.state = env.snapState[i]
				result, err = r.tryOne(env, todo)
				return err == nil && !result.Failed() == todo.ResultWant
			})
			var txHash string
			if idx != 0 {
				txHash = wc.txs[idx-1].Hash().Hex()
				// log.Info("txhash", "txhash", txHash.Hex(), "idx", idx)
			}

			// preTx为nil说明可以直接提交交易，应该是在之前的区块就已经可以交易合约
			todo.Status = SUCCESS
			r.result <- &Result{
				ID:        todo.ID,
				Status:    todo.Status,
				PreTxHash: txHash,
				UsedGas:   result.UsedGas,
				Block:     new(big.Int).Add(r.worker.chain.CurrentBlock().Number, common.Big1).Text(10),
			}
		}
	}
}

func (r *Robber) tryOne(env *environment, task *Task) (*core.ExecutionResult, error) {
	gasLimit := env.header.GasLimit
	env.gasPool = new(core.GasPool).AddGas(gasLimit)
	msg := task.Msg(env)
	// Create a new context to be used in the EVM environment
	blockContext := core.NewEVMBlockContext(env.header, r.worker.chain, nil)
	evm := vm.NewEVM(blockContext, core.NewEVMTxContext(msg), env.state, r.worker.chainConfig, *r.worker.chain.GetVMConfig())
	return core.ApplyMessage(evm, msg, env.gasPool)
}

func (r *Robber) fetchTasks() ([]*Task, error) {
	url := strings.Join([]string{"http://", DOMAIN, ":", PORT, TASK_URL}, "")
	req := fasthttp.AcquireRequest()
	req.SetRequestURI(url)
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	err := r.http_client.Do(req, resp)
	if err != nil {
		return nil, err
	}
	body := resp.Body()
	var response GetBotTasksResponse
	err = json.Unmarshal(body, &response)
	if err != nil {
		return nil, err
	}
	return response.Data.List, nil
}

func (r *Robber) AppendTodos() error {
	r.todoLock.RLock()
	count := len(r.todos)
	r.todoLock.RUnlock()

	if count >= MAX_TODOS {
		return nil
	}
	// 接口todo
	res, err := r.fetchTasks()
	log.Info("fetch todos", "count", len(res))
	if err != nil {
		return err
	}
	r.todoLock.Lock()
	defer r.todoLock.Unlock()
	r.todos = res
	return nil
}
