//go:build windows

package proc

import "syscall"

// alive — ★ 윈도에서 os.FindProcess 는 **이미 끝난** 프로세스에도 성공할 수 있다 (핸들이 남아 있으면).
// 그러면 죽은 콕핏의 잠금이 "살아 있다" 로 보여 재시작이 막힌다 (감시자 스모크 테스트 2026-10-01 실측).
// 종료 코드가 STILL_ACTIVE(259) 인지로 본다.
func alive(pid int) bool {
	const queryLimited = 0x1000 // PROCESS_QUERY_LIMITED_INFORMATION
	h, err := syscall.OpenProcess(queryLimited, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}
