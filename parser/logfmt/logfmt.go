package logfmt

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/qiniu/logkit/conf"
	"github.com/qiniu/logkit/parser"
	. "github.com/qiniu/logkit/parser/config"
	. "github.com/qiniu/logkit/utils/models"
)

func init() {
	parser.RegisterConstructor(TypeLogfmt, NewParser)
	parser.RegisterConstructor(TypeKeyValue, NewParser)
}

type Parser struct {
	name                 string
	keepString           bool
	disableRecordErrData bool
	numRoutine           int
	keepRawData          bool
	splitter             string
}

func NewParser(c conf.MapConf) (parser.Parser, error) {
	name, _ := c.GetStringOr(KeyParserName, "")
	disableRecordErrData, _ := c.GetBoolOr(KeyDisableRecordErrData, false)
	keepRawData, _ := c.GetBoolOr(KeyKeepRawData, false)
	splitter, _ := c.GetStringOr(KeySplitter, "=")
	keepString, _ := c.GetBoolOr(KeyKeepString, false)
	numRoutine := MaxProcs
	if numRoutine == 0 {
		numRoutine = 1
	}
	return &Parser{
		name:                 name,
		keepString:           keepString,
		disableRecordErrData: disableRecordErrData,
		numRoutine:           numRoutine,
		keepRawData:          keepRawData,
		splitter:             splitter,
	}, nil
}

func (p *Parser) Parse(lines []string) ([]Data, error) {
	if p.splitter == "" {
		p.splitter = "="
	}
	var (
		lineLen = len(lines)
		datas   = make([]Data, 0, lineLen)
		se      = &StatsError{}

		numRoutine = p.numRoutine
		sendChan   = make(chan parser.ParseInfo)
		resultChan = make(chan parser.ParseResult)
		wg         = new(sync.WaitGroup)
	)

	if lineLen < numRoutine {
		numRoutine = lineLen
	}

	for i := 0; i < numRoutine; i++ {
		wg.Add(1)
		go parser.ParseLineDataSlice(sendChan, resultChan, wg, true, p.parse)
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	go func() {
		for idx, line := range lines {
			sendChan <- parser.ParseInfo{
				Line:  line,
				Index: idx,
			}
		}
		close(sendChan)
	}()
	var parseResultSlice = make(parser.ParseResultSlice, lineLen)
	for resultInfo := range resultChan {
		parseResultSlice[resultInfo.Index] = resultInfo
	}

	se.DatasourceSkipIndex = make([]int, lineLen)
	datasourceIndex := 0
	for _, parseResult := range parseResultSlice {
		if len(parseResult.Line) == 0 {
			se.DatasourceSkipIndex[datasourceIndex] = parseResult.Index
			datasourceIndex++
			continue
		}

		if parseResult.Err != nil {
			se.AddErrors()
			se.LastError = parseResult.Err.Error()
			errData := make(Data)
			if !p.disableRecordErrData {
				errData[KeyPandoraStash] = parseResult.Line
			} else if !p.keepRawData {
				se.DatasourceSkipIndex[datasourceIndex] = parseResult.Index
				datasourceIndex++
			}
			if p.keepRawData {
				errData[KeyRawData] = parseResult.Line
			}
			if !p.disableRecordErrData || p.keepRawData {
				datas = append(datas, errData)
			}
			continue
		}
		if len(parseResult.Datas) == 0 { //数据为空时不发送
			se.LastError = fmt.Sprintf("parsed no data by line [%s]", parseResult.Line)
			se.AddErrors()
			continue
		}

		se.AddSuccess()
		if p.keepRawData {
			//解析后的部分数据对应全部的原始数据会造成比较严重的数据膨胀
			//TODO 减少膨胀的数据
			for i := range parseResult.Datas {
				parseResult.Datas[i][KeyRawData] = parseResult.Line
			}
		}
		datas = append(datas, parseResult.Datas...)
	}

	se.DatasourceSkipIndex = se.DatasourceSkipIndex[:datasourceIndex]
	if se.Errors == 0 && len(se.DatasourceSkipIndex) == 0 {
		return datas, nil
	}
	return datas, se
}

func (p *Parser) parse(line string) ([]Data, error) {

	pairs, err := splitKV(line, p.splitter)
	if err != nil {
		return nil, err
	}
	datas := make([]Data, 0, 100)

	// 调整数据类型
	for _, pair := range pairs {
		if len(pair)%2 == 1 {
			return nil, errors.New("key value not match")
		}
		field := make(Data)
		for i := 0; i < len(pair); i += 2 {
			// 消除双引号； 针对foo="" ,"foo=" 情况；其他情况如 a"b"c=d"e"f等首尾不出现引号的情况视作合法。
			kNum := strings.Count(pair[i], "\"")
			vNum := strings.Count(pair[i+1], "\"")
			if kNum%2 == 1 && vNum%2 == 1 {
				if strings.HasPrefix(pair[i], "\"") && strings.HasSuffix(pair[i+1], "\"") {
					pair[i] = pair[i][1:]
					pair[i+1] = pair[i+1][:len(pair[i+1])-1]
				}
			}
			if kNum%2 == 0 && len(pair[i]) > 1 {
				if strings.HasPrefix(pair[i], "\"") && strings.HasSuffix(pair[i], "\"") {
					pair[i] = pair[i][1 : len(pair[i])-1]
				}
			}
			if vNum%2 == 0 && len(pair[i+1]) > 1 {
				if strings.HasPrefix(pair[i+1], "\"") && strings.HasSuffix(pair[i+1], "\"") {
					pair[i+1] = pair[i+1][1 : len(pair[i+1])-1]
				}
			}

			if len(pair[i]) == 0 || len(pair[i+1]) == 0 {
				return nil, errors.New("no value was parsed after logfmt, will keep origin data in pandora_stash if disable_record_errdata field is false")
			}

			value := pair[i+1]
			if !p.keepString {
				if fValue, err := strconv.ParseFloat(value, 64); err == nil {
					field[pair[i]] = fValue
					continue
				}
				if bValue, err := strconv.ParseBool(value); err == nil {
					field[pair[i]] = bValue
					continue
				}

			}
			field[pair[i]] = value
		}
		if len(field) == 0 {
			continue
		}
		datas = append(datas, field)
	}

	// 修改数组顺序
	for i := 0; i < len(datas)/2; i++ {
		temp := datas[i]
		datas[i] = datas[len(datas)-i-1]
		datas[len(datas)-i-1] = temp
	}
	return datas, nil
}

func splitKV(line string, sep string) ([][]string, error) {
	line = strings.Replace(line, "\\\"", "", -1)

	data := make([][]string, 0, 100)
	// contain /n;
	// sep 被换行符分割
	if len(sep) > 1 {
		sepCount := strings.Count(line, sep)
		jointCount := strings.Count(strings.Replace(line, "\n", "", -1), sep)
		j := 1
		for sepCount < jointCount && j <= jointCount {
			tempLine := strings.Replace(line, "\n", "", j)
			tempCount := strings.Count(tempLine, sep)
			if tempCount > jointCount {
				jointCount = tempCount
				line = tempLine
			}
			j++
		}

	}

	nl := strings.Index(line, "\n")
	for nl != -1 {
		if nl >= len(line)-1 {
			line = line[:len(line)-1]
			break
		}
		preSep := strings.LastIndex(line[:nl], sep)
		nextSep := strings.Index(line[nl+1:], sep)
		// 前后没sep的情况 不用拆分
		if nextSep == -1 {
			break
		}

		if preSep == -1 {
			n := strings.Index(line[nl+1:], "\n")
			if n == -1 {
				break
			}
			nl = nl + 1 + n
			continue
		}

		// 前后都有sep的情况：右侧trim后有空格 合并；没有则不合并
		afTrim := strings.TrimSpace(line[nl+1 : nl+1+nextSep])
		nextSpace := strings.LastIndexFunc(afTrim, unicode.IsSpace)
		if nextSpace != -1 {
			n := strings.Index(line[nl+1:], "\n")
			if n == -1 {
				break
			}
			nl = nl + 1 + n
			continue
		}
		next := line[nl+1:]
		nextResult, err := splitKV(next, sep)
		if err != nil {
			return nil, err
		}
		data = append(data, nextResult...)
		line = line[:nl]
		nl = strings.Index(line, "\n")
	}

	line = strings.Replace(line, "\n", "", -1)
	if !strings.Contains(line, sep) {
		return nil, errors.New("no value was parsed after logfmt, will keep origin data in pandora_stash if disable_record_errdata field is false")
	}

	kvArr := make([]string, 0, 100)
	isKey := true
	vhead := 0
	lastSpace := 0
	pos := 0
	sepLen := len(sep)

	// key或value值中包含sep的情况；默认key中不包含sep；导致algorithm = 1+1=2会变成合法
	for pos+sepLen <= len(line) {
		if unicode.IsSpace(rune(line[pos : pos+1][0])) {
			nextSep := strings.Index(line[pos+1:], sep)
			if nextSep == -1 {
				break
			}
			if strings.TrimSpace(line[pos+1:pos+1+nextSep]) != "" {
				lastSpace = pos
				pos++
				continue
			}
		}
		if line[pos:pos+sepLen] == sep {
			if isKey {
				kvArr = append(kvArr, strings.TrimSpace(line[vhead:pos]))
				isKey = false
			} else {
				if lastSpace <= vhead {
					pos++
					continue
				}
				kvArr = append(kvArr, strings.TrimSpace(line[vhead:lastSpace]))
				kvArr = append(kvArr, strings.TrimSpace(line[lastSpace:pos]))
			}
			vhead = pos + sepLen
			pos = pos + sepLen - 1
		}
		pos++
	}
	if vhead < len(line) {
		kvArr = append(kvArr, strings.TrimSpace(line[vhead:]))
	}
	data = append(data, kvArr)
	return data, nil
}

func (p *Parser) Name() string {
	return p.name
}

func (p *Parser) Type() string {
	return TypeKeyValue
}
