import json, time, urllib.request, websocket
PORT=9333; BASE=f"http://127.0.0.1:{PORT}"
INJECT = r"""
(function(){
  if (window.__acpFull) return 'already';
  window.__acpFull = [];
  var dec = function(d){
    if (d && d.length!==undefined && typeof d[0]==='number') {
      try { return new TextDecoder().decode(new Uint8Array(d)); } catch(e){ return ''; }
    }
    return (typeof d==='string') ? d : '';
  };
  var keep = function(url){
    // 只保留与 AI 相关的请求，且完整保存，不做截断
    var of = window.fetch;
    window.fetch = function(input, init){
      try{
        var u = (typeof input==='string') ? input : (input && input.url) || '';
        if (String(u).indexOf('lightai') >= 0 || String(u).indexOf('assistant') >= 0) {
          window.__acpFull.push({v:'fetch', url:String(u), method:(init&&init.method)||'GET',
            headers:(init&&init.headers)||null, body:dec(init&&init.body)});
        }
      }catch(e){}
      return of.apply(this, arguments);
    };
    var I = window.__TAURI_INTERNALS__;
    if (I && typeof I.invoke === 'function') {
      var orig = I.invoke;
      I.invoke = function(){
        try{
          var a = arguments[1], cc = a && a.clientConfig;
          var u = cc ? String(cc.url||'') : '';
          if (u.indexOf('lightai') >= 0 || u.indexOf('assistant') >= 0) {
            window.__acpFull.push({v:'invoke', url:u, method:cc.method||'',
              headers:cc.headers||null, body:dec(cc.data)});
          }
        }catch(e){}
        return orig.apply(this, arguments);
      };
    }
  };
  keep();
  return 'installed3';
})()
"""
def get(p):
    try:
        with urllib.request.urlopen(BASE+p, timeout=2) as x: return json.load(x)
    except Exception: return None
page=None
for _ in range(20):
    for t in (get("/json/list") or []):
        if t.get("type")=="page":
            page=websocket.create_connection(t["webSocketDebuggerUrl"],timeout=10); break
    if page: break
seq=[0]
def ev(e):
    seq[0]+=1
    page.send(json.dumps({"id":seq[0],"method":"Runtime.evaluate","params":{"expression":e,"returnByValue":True}}))
    while True:
        m=json.loads(page.recv())
        if m.get("id")==seq[0]:
            r=m.get("result",{})
            if "exceptionDetails" in r: return "EXC "+json.dumps(r["exceptionDetails"])[:200]
            return r.get("result",{}).get("value")
print("inject3:", ev(INJECT))
