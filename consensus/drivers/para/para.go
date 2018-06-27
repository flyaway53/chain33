package para

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	log "github.com/inconshreveable/log15"
	//"gitlab.33.cn/chain33/chain33/common"
	"gitlab.33.cn/chain33/chain33/common/merkle"
	"gitlab.33.cn/chain33/chain33/consensus/drivers"
	"gitlab.33.cn/chain33/chain33/queue"
	"gitlab.33.cn/chain33/chain33/types"
	"gitlab.33.cn/chain33/chain33/util"
	"google.golang.org/grpc"
)

const (
	AddAct int64 = 1
	DelAct int64 = 2 //reference blockstore.go
)

var (
	plog                = log.New("module", "para")
	grpcSite            = "localhost:8802"
	currSeq       int64 = 0
	lastSeq       int64 = 0
	seqStep       int64 = 10 //experience needed
	blockedSeq    int64 = 0
	filterExec          = "ticket" //execName not decided
	txCacheSize   int64 = 10240
	blockSec      int64 = 10 //write block interval, second
	emptyBlockMin int64 = 2  //write empty block interval, minute
	zeroHash      [32]byte
	grpcRecSize   int   = 11 * 1024 * 1024 //the size should be limited in server
	seqRange      int64 = 5                // block txs in 5 seq
	emptyBlockSeq int64 = 100              //write empty block limit
)

type Client struct {
	*drivers.BaseClient
	conn       *grpc.ClientConn
	grpcClient types.GrpcserviceClient
	cache      *txCache
	lock       sync.RWMutex
}

func New(cfg *types.Consensus) *Client {
	c := drivers.NewBaseClient(cfg)
	grpcSite = cfg.ParaRemoteGrpcClient

	plog.Debug("New Para consensus client")

	msgRecvOp := grpc.WithMaxMsgSize(grpcRecSize)
	conn, err := grpc.Dial(grpcSite, grpc.WithInsecure(), msgRecvOp)

	if err != nil {
		panic(err)
	}
	grpcClient := types.NewGrpcserviceClient(conn)
	cache := newTxCache(txCacheSize)

	para := &Client{c, conn, grpcClient, cache, sync.RWMutex{}}

	c.SetChild(para)

	go para.ManageTxs()

	return para
}

//para 不检查任何的交易
func (client *Client) CheckBlock(parent *types.Block, current *types.BlockDetail) error {
	return nil
}

func (client *Client) ExecBlock(prevHash []byte, block *types.Block) (*types.BlockDetail, []*types.Transaction, error) {
	//exec block
	if block.Height == 0 {
		block.Difficulty = types.GetP(0).PowLimitBits
	}
	blockdetail, deltx, err := util.ExecBlock(client.GetQueueClient(), prevHash, block, false, true)
	if err != nil { //never happen
		return nil, deltx, err
	}
	//if len(blockdetail.Block.Txs) == 0 {
	//	return nil, deltx, types.ErrNoTx
	//}
	return blockdetail, deltx, nil
}

func (client *Client) FilterTxsForPara(Txs []*types.Transaction) []*types.Transaction {
	var txs []*types.Transaction
	for _, tx := range Txs {
		if string(tx.Execer) == filterExec {
			txs = append(txs, tx)
		}
	}
	return txs
}

//sequence start from 0 in blockchain
func (client *Client) GetCurrentSeq() int64 {
	//database or from txhash
	return 0
}

func (client *Client) SetTxs() {
	plog.Debug("Para consensus SetTxs")

	lastSeq, err := client.GetLastSeqOnMainChain()
	if err != nil {
		return
	}
	plog.Error("SetTxs", "LastSeq", lastSeq, "currSeq", currSeq, "blockedSeq", blockedSeq)
	if lastSeq >= currSeq {
		//debug phase
		if currSeq > 10 {
			return
		}

		blockSeq, _ := client.GetBlockHashFromMainChain(currSeq, currSeq)
		if blockSeq == nil {
			plog.Error("Not found block hash on seq", "start", currSeq, "end", currSeq)
			return
		}

		var hashes [][]byte
		for _, item := range blockSeq.Items {
			hashes = append(hashes, item.Hash)
			//break
		}

		blockDetails, _ := client.GetBlocksByHashesFromMainChain(hashes)
		if blockDetails == nil {
			plog.Error("GetBlockDetailerr")
			return
		}

		//protect the boundary
		if len(blockSeq.Items) != len(blockDetails.Items) {
			panic("")
			//plog.Error("GetBlockDetailerr")
			//return
		}

		for i, _ := range blockSeq.Items {

			opTy := blockSeq.Items[i].Type
			//blockHeight := blockDetails.Items[i].Block.Height
			txs := blockDetails.Items[i].Block.Txs
			//对每一个block进行操作，保留相关TX
			//为TX置标志位
			txs = client.FilterTxsForPara(txs)
			plog.Error("GetCurrentSeq", "Len of txs", len(txs), "ty", opTy)
			client.SetOpTxs(txs, opTy, currSeq)
		}

		currSeq += 1
	}
}

func (client *Client) SetOpTxs(txs []*types.Transaction, ty int64, currSeq int64) {
	if ty == AddAct {
		for i, _ := range txs {
			hash := txs[i].Hash()
			if client.cache.Exists(hash) {
				plog.Error("SetOpTxs", "err", "DupTx")
				continue
			}
			err := client.cache.Push(txs[i], currSeq)
			if err != nil {
				plog.Error("SetOpTxs AddAct", "err", err)
			}
		}
	} else if ty == DelAct {
		var height int64 = 1
		txMap := make(map[string]bool, 100)
		for i, _ := range txs {
			hash := txs[i].Hash()
			txinfo, err := client.queryTx(hash)
			if err != nil {
				plog.Error("SetOpTxs DelAct", "err", err)
				continue
			}
			if txinfo.Height > height {
				height = txinfo.Height
			}
			txMap[string(hash)] = true
		}
		// 不回退的交易需要重新打包
		block, _ := client.RequestBlock(height)
		newTxs := make([]*types.Transaction, 0, len(block.Txs))
		for i, _ := range block.Txs {
			tx := block.Txs[i]
			if _, ok := txMap[string(tx.Hash())]; !ok {
				newTxs = append(newTxs, tx)
			}
		}

		// wait for remaining txs in cache to be blocked
		for client.cache.Size() != 0 {
			plog.Info(fmt.Sprintf("%d txs remain to be blocked", client.cache.Size()))
			time.Sleep(time.Second)
		}

		// create fork point
		lastBlock, _ := client.RequestBlock(height - 1)
		if len(newTxs) != 0 {
			client.createBlock(lastBlock, newTxs)
		} else {
			client.SetCurrentBlock(lastBlock)
		}
	} else {
		plog.Error("SetOpTxs", "err", "Incorrect block type")
	}
}

func (client *Client) queryTx(hash []byte) (*types.TransactionDetail, error) {
	msg := client.GetQueueClient().NewMessage("blockchain", types.EventQueryTx, &types.ReqHash{hash})
	err := client.GetQueueClient().Send(msg, true)
	if err != nil {
		return nil, err
	}
	resp, err := client.GetQueueClient().Wait(msg)
	if err != nil {
		return nil, err
	}
	return resp.Data.(*types.TransactionDetail), nil
}

func (client *Client) MonitorTxs() {
	plog.Error("MonitorTxs", "len for txs", client.cache.Size())
}

func (client *Client) ManageTxs() {
	//during start
	currSeq = client.GetCurrentSeq()
	plog.Error("Para consensus ManageTxs")
	for {
		time.Sleep(time.Second)
		client.SetTxs()
		client.MonitorTxs()
	}

}

func (client *Client) GetLastSeqOnMainChain() (int64, error) {
	seq, err := client.grpcClient.GetLastBlockSequence(context.Background(), &types.ReqNil{})
	if err != nil {
		plog.Error("GetLastSeqOnMainChain", "Error", err.Error())
		return -1, err
	}
	//the reflect checked in grpcHandle
	return seq.Data, nil
}

func (client *Client) GetBlocksByHashesFromMainChain(hashes [][]byte) (*types.BlockDetails, error) {
	req := &types.ReqHashes{hashes}
	blocks, err := client.grpcClient.GetBlockByHashes(context.Background(), req)
	if err != nil {
		plog.Error("GetBlocksByHashesFromMainChain", "Error", err.Error())
		return nil, err
	}
	return blocks, nil
}

func (client *Client) GetBlockHashFromMainChain(start int64, end int64) (*types.BlockSequences, error) {
	req := &types.ReqBlocks{start, end, true, []string{}}
	blockSeq, err := client.grpcClient.GetBlockSequences(context.Background(), req)
	if err != nil {
		plog.Error("GetBlockHashFromMainChain", "Error", err.Error())
		return nil, err
	}
	return blockSeq, nil
}

func (client *Client) Close() {
	//清空交易
	plog.Info("consensus para closed")
}

func (client *Client) CreateGenesisTx() (ret []*types.Transaction) {
	var tx types.Transaction
	tx.Execer = []byte("coins")
	tx.To = client.Cfg.Genesis
	//gen payload
	g := &types.CoinsAction_Genesis{}
	g.Genesis = &types.CoinsGenesis{}
	g.Genesis.Amount = 1e8 * types.Coin
	tx.Payload = types.Encode(&types.CoinsAction{Value: g, Ty: types.CoinsActionGenesis})
	ret = append(ret, &tx)
	return
}

func (client *Client) ProcEvent(msg queue.Message) bool {
	return false
}

//从txOps拿交易
//正常情况下，打包交易
//如果有del标识，先删除原来区块，重新打包
//需要更新txOps
func (client *Client) CreateBlock() {
	issleep := true
	count := 0
	for {
		//don't check condition for block caughtup
		if !client.IsMining() {
			time.Sleep(time.Second)
			continue
		}
		if issleep {
			time.Sleep(time.Second * time.Duration(blockSec))
			count++
		}
		if count >= int(emptyBlockMin*60/blockSec) && currSeq-blockedSeq > emptyBlockSeq {
			plog.Info("Create an empty block")
			block := client.GetCurrentBlock()
			emptyBlock := &types.Block{}
			emptyBlock.StateHash = block.StateHash
			emptyBlock.ParentHash = block.Hash()
			emptyBlock.Height = block.Height + 1
			emptyBlock.Difficulty = types.GetP(0).PowLimitBits
			emptyBlock.Txs = nil
			emptyBlock.TxHash = zeroHash[:]
			emptyBlock.BlockTime = time.Now().Unix()

			er := client.WriteBlock(block.StateHash, emptyBlock)
			if er != nil {
				plog.Error(fmt.Sprintf("********************err:%v", er.Error()))
				continue
			}
			client.SetCurrentBlock(emptyBlock)
			count = 0
		}

		lastBlock := client.GetCurrentBlock()
		txs := client.RequestTx(int(types.GetP(lastBlock.Height+1).MaxTxNumber), seqRange, nil)
		if len(txs) == 0 {
			issleep = true
			continue
		}
		issleep = false
		//check dup
		//txs = client.CheckTxDup(txs)
		err := client.createBlock(lastBlock, txs)
		if err != nil {
			issleep = true
			continue
		}
		time.Sleep(time.Second * time.Duration(blockSec))
	}
}

func (client *Client) createBlock(lastBlock *types.Block, txs []*types.Transaction) error {
	var newblock types.Block
	plog.Error(fmt.Sprintf("the len txs is: %v", len(txs)))
	newblock.ParentHash = lastBlock.Hash()
	newblock.Height = lastBlock.Height + 1
	client.AddTxsToBlock(&newblock, txs)
	//挖矿固定难度
	newblock.Difficulty = types.GetP(0).PowLimitBits
	newblock.TxHash = merkle.CalcMerkleRoot(newblock.Txs)
	newblock.BlockTime = time.Now().Unix()
	if lastBlock.BlockTime >= newblock.BlockTime {
		newblock.BlockTime = lastBlock.BlockTime + 1
	}
	err := client.WriteBlock(lastBlock.StateHash, &newblock)
	if err != nil {
		plog.Error(fmt.Sprintf("********************err:%v", err.Error()))
		return err
	}
	return nil
}

// 向cache获取交易
func (client *Client) RequestTx(size int, seqRange int64, txHashList [][]byte) []*types.Transaction {
	plog.Error("Get Txs from txOps")
	return client.cache.Pull(size, seqRange, txHashList)
}

// 向blockchain写区块
func (client *Client) WriteBlock(prev []byte, block *types.Block) error {
	plog.Error("write block in parachain")
	var deltxSeq int64 = 0
	blockdetail, deltx, err := client.ExecBlock(prev, block)
	if len(deltx) > 0 {
		deltxSeq = client.DelTxs(deltx)
	}
	if err != nil {
		return err
	}
	msg := client.GetQueueClient().NewMessage("blockchain", types.EventAddBlockDetail, blockdetail)
	client.GetQueueClient().Send(msg, true)
	resp, err := client.GetQueueClient().Wait(msg)
	if err != nil {
		return err
	}

	if resp.GetData().(*types.Reply).IsOk {
		client.SetCurrentBlock(block)
		txSeq := client.DelTxs(block.Txs)
		// 成功打包区块才更新blockedSeq
		if txSeq > deltxSeq {
			client.SetBlockedSeq(txSeq)
		} else {
			client.SetBlockedSeq(deltxSeq)
		}
	} else {
		reply := resp.GetData().(*types.Reply)
		return errors.New(string(reply.GetMsg()))
	}
	return nil
}

// 向cache删除交易
func (client *Client) DelTxs(deltx []*types.Transaction) (seq int64) {
	for i := 0; i < len(deltx); i++ {
		exist := client.cache.Exists(deltx[i].Hash())
		if exist {
			if i == len(deltx)-1 {
				seq = client.cache.Get(deltx[i].Hash()).seq
			}
			client.cache.Remove(deltx[i].Hash())
		}
	}
	return
}

// 保存blockedSeq
func (client *Client) SetBlockedSeq(seq int64) error {
	client.lock.Lock()
	defer client.lock.Unlock()
	if seq > blockedSeq {
		blockedSeq = seq
		// 持久化存储用于恢复
	}
	return nil
}
