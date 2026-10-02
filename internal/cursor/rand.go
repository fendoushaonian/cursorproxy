package cursor

import "crypto/rand"

// cryptoRand 抽出来是为了测试里替换失败路径，生产就是 crypto/rand.Read。
var cryptoRand = rand.Read
