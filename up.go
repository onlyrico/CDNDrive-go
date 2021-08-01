package main

import (
	"CDNDrive/drivers"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// /tmp/cdndrive-go.conf 保存用户 cookie 信息

type userCookieJson struct {
	Cookie map[string]string
	fp     *os.File
}

func loadUserCookie() *userCookieJson {
	v := &userCookieJson{Cookie: make(map[string]string)}

	//加载不到也没事
	path, _ := os.UserConfigDir()
	f, err := os.OpenFile(filepath.Join(path, "cdndrive-go.conf"), os.O_RDWR|os.O_CREATE, 0644)
	if err == nil {
		json.NewDecoder(f).Decode(v)
	}
	v.fp = f

	return v
}

func (c *userCookieJson) getCookieByDriverName(name string) string {
	if _debug {
		fmt.Println("Finding cookie for " + name)
	}
	for driver, cookie := range c.Cookie {
		if driver == name {
			if _debug {
				fmt.Println("Cookie is", cookie)
			}
			return cookie
		}
	}
	return ""
}

func (c *userCookieJson) setDriveCookie(name, cookie string, skipCheck bool) (err error) {
	//检查cookie有效性
	if !skipCheck {
		var ok bool
		if ok, err = getDriverByName(name).CheckCookie(cookie); !ok {
			return
		}
	}

	c.Cookie[name] = cookie
	err = json.NewEncoder(c.fp).Encode(c)
	if err != nil {
		return
	}
	err = c.fp.Sync()
	return
}

func HandlerUpload(args []string, ds map[string]drivers.Driver, threadN int, blockSize int) {
	txt_uploadFail := "<fg=black;bg=red>上传失败：</>"

	cookieJson := loadUserCookie()

	//打开文件，读取信息
	//TODO 上传多个文件？
	f, err := os.OpenFile(args[0], os.O_RDONLY, 0)
	if err != nil {
		colorLogger.Println(txt_uploadFail, err.Error())
		return
	}
	fs, _ := f.Stat()
	fileSize := fs.Size()
	fileName := filepath.Base(f.Name())
	fileName_display := "<yellow>" + fileName + "</>"

	//分块计算
	driverN := len(ds)
	blockN := int(math.Ceil(float64(fileSize) / float64(blockSize)))
	blocks_dict := make([]metaJSON_Block, blockN)
	var _offset int64
	for i, _ := range blocks_dict {
		blocks_dict[i].offset = _offset
		blocks_dict[i].i = i
		if i == blockN-1 { //最后一块
			blocks_dict[i].Size = int(fileSize - int64(_offset))
		} else {
			blocks_dict[i].Size = blockSize
		}
		_offset += int64(blockSize)

		//Sha1给下面worker算？

		if false { //TODO 续传
			continue
		}
	}
	colorLogger.Println("<fg=black;bg=green>正在上传：</><yellow>", f.Name(), "</>大小", ConvertFileSize(fileSize), "分块数", blockN, "分块大小", blockSize, "正在计算 sha1sum")
	if fileSize <= 0 {
		return
	}

	//计算checksum
	hasher := sha1.New()
	f.Seek(0, 0)
	if _, err := io.Copy(hasher, io.NewSectionReader(f, 0, 4*1024*1024)); err != nil {
		colorLogger.Println(txt_uploadFail, "计算 sha1sum 错误：", err.Error())
		return
	}
	sha1_4m := hasher.Sum(nil)
	hasher.Reset()

	f.Seek(0, 0)
	if _, err := io.Copy(hasher, f); err != nil {
		colorLogger.Println(txt_uploadFail, "计算 sha1sum 错误：", err.Error())
		return
	}
	sha1_all := hasher.Sum(nil)
	f.Seek(0, 0)

	sha1sum := fmt.Sprintf("%x", sha1_all)
	sha1_4m = sha1_4m //TODO save history

	//发送任务
	lock := &sync.Mutex{}
	ctx, cancel := context.WithCancel(context.Background())

	chanTasks := make([]chan *metaJSON_Block, driverN)
	chanStatus := make([]chan int, driverN)
	finishMaps := make([][]bool, driverN)
	finishurls := make([][]string, driverN)
	ctxs := make([]context.Context, driverN)
	cancels := make([]context.CancelFunc, driverN)
	for i, _ := range chanTasks {
		chanTasks[i] = make(chan *metaJSON_Block, blockN)
		chanStatus[i] = make(chan int)
		finishMaps[i] = make([]bool, blockN)
		finishurls[i] = make([]string, blockN)
		ctxs[i], cancels[i] = context.WithCancel(ctx)

		for j, _ := range blocks_dict {
			chanTasks[i] <- &blocks_dict[j]
		}
	}

	//上面的工作只用做一次，下面开始对各个driver上传
	var ii = 0
	wg := &sync.WaitGroup{}
	wg.Add(driverN)
	for _, _d := range ds {
		cookie := cookieJson.getCookieByDriverName(_d.Name())

		//上传进度控制
		go func(d drivers.Driver, ii int) {
			var finishedBlockCounter int
			time_start := time.Now()
			defer wg.Done()
			for {
				select {
				case <-ctxs[ii].Done():
					return
				case finishedBlockID := <-chanStatus[ii]:
					if finishedBlockID < 0 { //负数是出错代码，此时该driver退出
						colorLogger.Println(txt_uploadFail, d.DisplayName(), fileName_display)
						cancels[ii]()
						return
					}

					finishMaps[ii][finishedBlockID] = true
					finishedBlockCounter++
					//TODO 上传在这里保存进度请

					if finishedBlockCounter == blockN {
						colorLogger.Println(d.DisplayName(), "上传完成，开始编码并上传索引图片。")

						//这个是要上传的meta
						blocks_dict_copy := make([]metaJSON_Block, blockN)
						for i, _ := range blocks_dict_copy {
							blocks_dict_copy[i].i = i
							blocks_dict_copy[i].Sha1 = blocks_dict[i].Sha1
							blocks_dict_copy[i].Size = blocks_dict[i].Size
							blocks_dict_copy[i].URL = finishurls[ii][i]
						}
						v := &metaJSON{
							Time:       time.Now().Unix(),
							FileName:   fileName,
							Size:       fileSize,
							Sha1:       sha1sum,
							BlockDicts: blocks_dict_copy,
						}
						data, _ := json.Marshal(v)

						try_max := 10
						for i := 0; i < try_max; i++ { //尝试10次
							url, err := d.Upload(data, ctx, http.DefaultClient, cookie, sha1sum)

							if err != nil {
								if i < try_max-1 {
									colorLogger.Println(d.DisplayName(), "索引图片第", i+1, "次上传失败，重试。")
								} else {
									colorLogger.Println(d.DisplayName(), "索引图片第", i+1, "次下载失败，不重试，文件上传失败。")
									go func() { chanStatus[ii] <- -2 }()
									break
								}
							} else {
								seconds := time.Now().Sub(time_start).Seconds()
								colorLogger.Println(d.DisplayName(), fileName_display, "上传完毕，用时", seconds, "秒，平均速度", ConvertFileSize(int64(float64(fileSize)/seconds)))
								colorLogger.Println(d.DisplayName(), fileName_display, "上传完毕 <green>META URL</> ->", d.Real2Meta(url))
								cancels[ii]()
								return
							}
						}
					}
				}
			}
		}(_d, ii)

		for j := 0; j < threadN; j++ {
			go worker_up(chanTasks[ii], chanStatus[ii], ctxs[ii], j, cookie, _d, f, lock, finishMaps[ii], finishurls[ii])
		}

		ii++
	}

	wg.Wait()
	cancel()
}

func worker_up(chanTask chan *metaJSON_Block, chanStatus chan int, ctx context.Context, workerID int, cookie string, d drivers.Driver, f *os.File, lock *sync.Mutex, finishMap []bool, finishurls []string) {
	client := &http.Client{}
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-chanTask:
			try_max := 10
			for i := 0; i < try_max; i++ { //尝试10次
				err := func() (err error) {
					//读取文件内容
					lock.Lock()
					f.Seek(task.offset, 0)
					data := make([]byte, task.Size)
					_, err = f.Read(data)
					if err != nil {
						return
					}
					lock.Unlock()

					//sha1sum 只计算一次
					var sha1sum string
					if task.Sha1 == "" { //好像这样还是会多次计算...
						sha1sum = fmt.Sprintf("%x", sha1.Sum(data))
						task.Sha1 = sha1sum
					} else {
						sha1sum = task.Sha1
					}

					//防卡？
					ctx2, cancel := context.WithDeadline(ctx, time.Now().Add(time.Minute))

					//上传，这里task共用所以不能设置url
					finishurls[task.i], err = d.Upload(data, ctx2, client, cookie, sha1sum)

					cancel()
					return
				}()
				if err != nil {
					colorLogger.Println(d.DisplayName(), "分块", task.i+1, "<red>错误代码：</>", err.Error())
					if i < try_max-1 {
						colorLogger.Println(d.DisplayName(), "分块", task.i+1, "第", i+1, "次上传失败，重试。")
					} else {
						colorLogger.Println(d.DisplayName(), "分块", task.i+1, "第", i+1, "次下载失败，不重试，文件上传失败。")
						chanStatus <- -1 //停止代码 -1 上传失败
					}
				} else {
					chanStatus <- task.i
					colorLogger.Println(d.DisplayName(), "分块", task.i+1, "/", len(finishurls), "上传成功。")
					break
				}
			}
		}
	}
}
