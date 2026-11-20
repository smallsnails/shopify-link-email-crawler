# 本地测试服务器：无扩展名路径自动回退到 .html（模拟真实站点的 Clean URL）
import os
from http.server import SimpleHTTPRequestHandler, HTTPServer

class Handler(SimpleHTTPRequestHandler):
    def translate_path(self, path):
        p = super().translate_path(path)
        if not os.path.exists(p):
            base = path.split("?", 1)[0].split("#", 1)[0].rstrip("/")
            alt = super().translate_path(base + ".html")
            if os.path.exists(alt):
                return alt
        return p

if __name__ == "__main__":
    HTTPServer(("127.0.0.1", 8099), Handler).serve_forever()
