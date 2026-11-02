import ctypes
from ctypes import wintypes
import sys

user32 = ctypes.windll.user32
kernel32 = ctypes.windll.kernel32

def find_windows(classname=None, title=None):
    result = []
    @ctypes.WINFUNCTYPE(ctypes.c_bool, wintypes.HWND, wintypes.LPARAM)
    def cb(hwnd, lparam):
        buf = ctypes.create_unicode_buffer(256)
        user32.GetClassNameW(hwnd, buf, 256)
        if buf.value == classname:
            result.append(hwnd)
        return True
    user32.EnumWindows(cb, 0)
    return result

# 64 位进程读取 TBBUTTON 需要 ReadProcessMemory
TH32 = None
processes = {}
def get_pid(hwnd):
    pid = wintypes.DWORD()
    user32.GetWindowThreadProcessId(hwnd, ctypes.byref(pid))
    return pid.value

def read_tray_buttons(toolbar_hwnd):
    """读 ToolbarWindow32 的按钮文字（tray 图标的 tooltip）"""
    count = user32.SendMessageW(toolbar_hwnd, 0x0418, 0, 0)  # TB_BUTTONCOUNT
    pid = get_pid(toolbar_hwnd)
    PROCESS_VM_OPERATION = 0x8; PROCESS_VM_READ = 0x10
    hproc = kernel32.OpenProcess(PROCESS_VM_OPERATION | PROCESS_VM_READ, False, pid)
    out = []
    # TBBUTTON 64 位: 20 字节结构（idCommand 4, fsState 1, fsStyle 1, bReserved 2, dwData 8, iString 8）→ 补齐 32
    TBBUTTON_SIZE = 32
    for i in range(count):
        tb = ctypes.create_string_buffer(TBBUTTON_SIZE)
        # TB_GETBUTTON 需要 dest 缓冲在目标进程 → VirtualAllocEx + WriteProcessMemory
        remote = kernel32.VirtualAllocEx(hproc, None, TBBUTTON_SIZE + 512, 0x3000, 0x04)
        user32.SendMessageW(toolbar_hwnd, 0x0417, i, remote)  # TB_GETBUTTON
        buf = ctypes.create_string_buffer(TBBUTTON_SIZE + 512)
        kernel32.ReadProcessMemory(hproc, ctypes.c_void_p(remote), buf, TBBUTTON_SIZE + 512, None)
        iString = int.from_bytes(buf.raw[16:24], "little")
        if iString:
            txt = ctypes.create_unicode_buffer(256)
            kernel32.ReadProcessMemory(hproc, ctypes.c_void_p(iString), txt, 512, None)
            out.append(txt.value)
        kernel32.VirtualFreeEx(hproc, remote, 0, 0x8000)
    kernel32.CloseHandle(hproc)
    return out

found = []
# 主托盘 + 溢出区
for cls in ("Shell_TrayWnd", "NotifyIconOverflowWindow"):
    for top in find_windows(cls):
        @ctypes.WINFUNCTYPE(ctypes.c_bool, wintypes.HWND, wintypes.LPARAM)
        def child_cb(ch, lp):
            cbuf = ctypes.create_unicode_buffer(256)
            user32.GetClassNameW(ch, cbuf, 256)
            if cbuf.value == "ToolbarWindow32":
                items = read_tray_buttons(ch)
                for it in items:
                    found.append(f"{cls}: {it}")
            return True
        user32.EnumChildWindows(top, child_cb, 0)

for f in found:
    print(f)
print("---")
pb = [f for f in found if "PhotoBeam" in f or "photobeam" in f.lower()]
print("PhotoBeam tray icon:", "FOUND: " + "; ".join(pb) if pb else "NOT FOUND")
