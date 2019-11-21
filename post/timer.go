---
title: " 定时器实现原理"
date: 2019-11-17T10:46:41+08:00
draft: false
categories: ["go"]
author: "wei.tan"
---

## timer

```
type Timer struct {
    //channel, 面向应用层
	C <-chan Time
	//系统定时器, 面向底层实现
	r runtimeTimer
}
```

**实现原理**:     

一个进程中多个 timer 由同一个系统协程来管理, 系统协程把每一个 timer 存放在四叉堆中, 按照 when 排序, 过期触发 f 执行

![](http://i.caigoubao.cc/626957/timer_02.png)

系统定时器:
```
type runtimeTimer struct {
	tb uintptr  //存储当前定时器的数组
	i  int      //当前定时器下标

	when   int64    //定时器触发时间
	period int64    //定时器触发周期, 对 timer 来说为 0 
	f      func(interface{}, uintptr) //定时器触发回调函数
	arg    interface{}  //回调参数
	seq    uintptr  //回调参数
}
```

创建定时器:
```
func NewTimer(d Duration) *Timer {
	c := make(chan Time, 1)
	t := &Timer{
		C: c,
		r: runtimeTimer{
			when: when(d),
			f:    sendTime,
			arg:  c,
		},
	}
	//将 runtimeTimer 放到系统协程, 如果没有系统协程, 开启一个
	startTimer(&t.r)
	return t
}
```


定时器触发操作:

为了使 ticker 能够复用 timer, channel 设置为有缓冲, 搭配 default    
    
    
ticker 周期发送事件, 即使上一次的数据没有取走时, 也不会阻塞协程.  下一次事件到来, 如果 channel 中有值, 则不会向 channel 中写数据, 直接丢弃事件
```
func sendTime(c interface{}, seq uintptr) {
	// Non-blocking send of time on c.
	// Used in NewTimer, it cannot block anyway (buffer).
	// Used in NewTicker, dropping sends on the floor is
	// the desired behavior when the reader gets behind,
	// because the sends are periodic.
	select {
	case c.(chan Time) <- Now():
	default:
	}
}

```

停止定时器:

从系统协程中移除 timer, 但是不关闭 channel, 防止用户读数据
```
func (t *Timer) Stop() bool {
    return stopTimer(&t.r)
}
```

重置定时器:
先停止、后新建



## ticker

数据结构和 timer 一样
```
type Ticker struct {
    C <-chan Time
    r runtimeTimer
}
```

数据接口
```
func NewTicker(d Duration) *Ticker
func (t *Ticker) Stop()
```

for range 持续从 channel 中读取数据, 如果没有就会阻塞

```
// TickerDemo 用于演示ticker基础用法
func TickerDemo() {
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()

    for range ticker.C {
        log.Println("Ticker tick.")
    }
}
```


注意, 每次创建之后需要释放, 不然容易泄露    
下面每次 for 循环都创建了一个新的 ticker, 但是没有释放, 会导致大量 ticker 消耗 cpu
```
func WrongTicker() {
    for {
        select {
        case <-time.Tick(1 * time.Second):
            log.Printf("Resource leak!")
        }
    }
}
```


## 实现原理

![](http://i.caigoubao.cc/626957/ticker_01.png)


```
type timersBucket struct {
    lock         mutex  //操作四叉堆, 需要加锁
    gp           *g          // 处理堆中事件的协程
    created      bool        // 事件处理协程是否已创建，默认为false，添加首个定时器时置为true
    sleeping     bool        // 事件处理协程（gp）是否在睡眠(如果t中有定时器，还未到触发的时间，那么gp会投入睡眠)
    rescheduling bool        // 事件处理协程（gp）是否已暂停（如果t中定时器均已删除，那么gp会暂停）
    sleepUntil   int64       // 事件处理协程睡眠时间
    waitnote     note        // 事件处理协程睡眠事件（据此唤醒协程）
    t            []*timer    // 定时器切片
}
```

一个 timersBucket 包含了多个定时器, 并且对应一个系统协程    

当定时器比较多时, 为了充分利用 cpu, 可以设置多个 timersBucket, go 默认 64 个
![](http://i.caigoubao.cc/626957/b1_01.png)


创建定时器:    
- 分配 timersBucket
- 创建 timer
- 加入 timersBucket 堆尾部, 依照触发时间进行堆排序, 小顶堆
- 如果刚加入的 timer 到达堆顶, 需要立即处理, 会唤醒系统协程


**timerproc**    
系统协程, 首次创建定时器时启动, 一旦创建永不销毁, 读出堆顶定时器时间, 计算睡眠时间, 进入睡眠状态, 时间到了就被唤醒触发事件
