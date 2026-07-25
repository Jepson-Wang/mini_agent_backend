package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

/*
你的 Go 网关要同时调用 N 个 agent 服务，把结果聚合返回。要求：

并发调用所有下游，不能串行
总超时 3 秒（无论下游多少个）
任一下游失败 → 立即取消其余，不浪费资源
全部成功 → 返回按输入顺序排列的结果切片
有失败 → 返回第一个 error
不能有 goroutine 泄漏
不能有数据竞争（go test -race 要过）
*/
/*
注意的点:
超时 ctx 有没有 defer cancel()
失败时其余任务是真的被取消了，还是只是"我不等了"
结果收集有没有数据竞争
有没有 goroutine 泄漏（尤其是提前返回时，还在跑的 goroutine 往哪写）
循环变量捕获（你现在应该是 1.22+，但我会看你有没有意识）
error 是不是第一个发生的那个
结果顺序是否和输入对应
*/

type Request struct {
	ID   int
	Name string
}

type Result struct {
	ID      int
	Content string
}

// 模拟下游调用：耗时随机，ID 为 3 时必定失败
func callService(ctx context.Context, req Request) (Result, error) {
	delay := time.Duration(rand.Intn(1000)+200) * time.Millisecond

	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-time.After(delay):
	}

	if req.ID == 3 {
		return Result{}, errors.New("service 3 unavailable")
	}
	return Result{ID: req.ID, Content: fmt.Sprintf("resp-from-%s", req.Name)}, nil
}

// ===== 你要实现的部分 =====
func FanOut(ctx context.Context, reqs []Request) ([]Result, error) {
	// 开发思路：
	//要求并发调用，所以要使用匿名函数然后for循环并发执行
	// ctx设置超时时间,如果达到了3s，那就取消
	//目前我是通过errgroup做同步取消的，但现在需要离开errgroup来自主实现
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	var firstErr error
	var once sync.Once
	defer cancel()
	var wg sync.WaitGroup
	results := make([]Result, len(reqs))
	for i, req := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := callService(ctx, req)
			if err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}
			results[i] = res
		}()
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}

	return results, nil

}

// ========================

func main() {
	reqs := []Request{
		{1, "alpha"}, {2, "beta"}, {3, "gamma"}, {4, "delta"},
	}

	results, err := FanOut(context.Background(), reqs)
	fmt.Println("results:", results)
	fmt.Println("err:", err)
}
