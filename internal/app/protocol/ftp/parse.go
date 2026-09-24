package ftp

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"nas-agent/internal/app/model"
)

// parseLIST 解析传统 LIST 输出（兼容 UNIX 与 DOS 风格）
func parseLIST(data, dir string) []model.FileInfo {
	var out []model.FileInfo
	now := time.Now()
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "total ") {
			continue
		}
		var fi model.FileInfo
		switch {
		case isUnixLine(line):
			fi = parseUnixLine(line, now)
		case isDOSLine(line):
			fi = parseDOSLine(line)
		default:
			continue
		}
		if fi.Name == "" || fi.Name == "." || fi.Name == ".." {
			continue
		}
		fi.Path = path.Join(path.Clean("/"+dir), fi.Name)
		out = append(out, fi)
	}
	return out
}

func isUnixLine(line string) bool {
	if len(line) < 10 {
		return false
	}
	c := line[0]
	return c == '-' || c == 'd' || c == 'l' || c == 'b' || c == 'c' || c == 'p' || c == 's'
}

func parseUnixLine(line string, now time.Time) model.FileInfo {
	fields := strings.Fields(line)
	fi := model.FileInfo{IsDir: line[0] == 'd'}
	if len(fields) < 8 {
		return fi
	}
	size, _ := strconv.ParseInt(fields[4], 10, 64)
	fi.Size = size
	// 日期字段可能是 "Sep 12 13:45" 或 "Sep 12 2021"
	dateStr := fields[5] + " " + fields[6] + " " + fields[7]
	if t, err := time.Parse("Jan _2 15:04", dateStr); err == nil {
		t = t.AddDate(now.Year(), 0, 0)
		if t.After(now.Add(365 * 24 * time.Hour)) {
			t = t.AddDate(-1, 0, 0)
		}
		fi.ModTime = t
	} else if t, err := time.Parse("Jan _2 2006", dateStr); err == nil {
		fi.ModTime = t
	}
	name := strings.Join(fields[8:], " ")
	// 符号链接 name -> target
	if i := strings.Index(name, " -> "); i >= 0 {
		name = name[:i]
	}
	fi.Name = name
	return fi
}

func isDOSLine(line string) bool {
	// MM-DD-YY HH:MM(AM/PM) <DIR>/size name
	if len(line) < 12 {
		return false
	}
	if line[2] != '-' || line[5] != '-' {
		return false
	}
	return true
}

func parseDOSLine(line string) model.FileInfo {
	fi := model.FileInfo{}
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return fi
	}
	// 01-02-06  03:04PM  <DIR>  name 或 01-02-06  03:04PM  1234 name
	if t, err := time.Parse("01-02-06 3:04PM", fields[0]+" "+fields[1]); err == nil {
		fi.ModTime = t
	}
	idx := 2
	if strings.EqualFold(fields[2], "<DIR>") {
		fi.IsDir = true
		idx = 3
	} else {
		sizeStr := strings.ReplaceAll(fields[2], ",", "")
		fi.Size, _ = strconv.ParseInt(sizeStr, 10, 64)
		idx = 3
	}
	fi.Name = strings.Join(fields[idx:], " ")
	return fi
}

// statMLSTLocked 调用 MLST，调用方需持有 c.mu
func (c *Client) statMLSTLocked(p string) (*model.FileInfo, error) {
	code, msg, err := c.cmd("MLST %s", c.fullPath(p))
	if err != nil || code/100 != 2 {
		return nil, fmt.Errorf("mlst failed: %d %s %v", code, msg, err)
	}
	// 应答中含一行 " facts=; name"
	for _, line := range strings.Split(msg, "\n") {
		if strings.Contains(line, ";") && strings.Contains(line, " ") {
			sp := strings.Index(line, " ")
			if sp < 0 {
				continue
			}
			facts := strings.TrimSpace(line[:sp])
			name := strings.TrimSpace(line[sp+1:])
			if name == "" {
				continue
			}
			fi := model.FileInfo{
				Name: path.Base(strings.TrimSuffix(name, "/")),
				Path: path.Clean("/" + p),
			}
			for _, kv := range strings.Split(strings.TrimSuffix(facts, ";"), ";") {
				eq := strings.Index(kv, "=")
				if eq < 0 {
					continue
				}
				k := strings.ToLower(kv[:eq])
				v := kv[eq+1:]
				switch k {
				case "type":
					fi.IsDir = strings.EqualFold(v, "dir")
				case "size":
					fi.Size, _ = strconv.ParseInt(v, 10, 64)
				case "modify":
					if t, err := time.Parse("20060102150405", v); err == nil {
						fi.ModTime = t
					}
				}
			}
			return &fi, nil
		}
	}
	return nil, fmt.Errorf("ftp: MLST parse failed")
}

// Stat 查询单个文件信息
func (c *Client) Stat(p string) (*model.FileInfo, error) {
	c.mu.Lock()
	fi, err := c.statMLSTLocked(p)
	c.mu.Unlock()
	if err == nil {
		return fi, nil
	}
	// 回退：列父目录
	dir, name := path.Split(path.Clean("/" + p))
	list, err := c.List(dir)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("ftp: %s not found", p)
}
