package webui

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	adminUser = "vera@1030"
	adminPass = "XTa4IsndYQe_NdEy"
)

type Server struct {
	params      *LiveParams
	events      *EventLog
	serviceName string
}

func NewServer(params *LiveParams, events *EventLog, serviceName string) *Server {
	if serviceName == "" {
		serviceName = "eth-perp-system"
	}
	return &Server{params: params, events: events, serviceName: serviceName}
}

func (s *Server) Listen(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.auth(s.handleUI))
	mux.HandleFunc("/api/params", s.auth(s.handleParams))
	mux.HandleFunc("/api/params/reset", s.auth(s.handleReset))
	mux.HandleFunc("/api/params/initialize", s.auth(s.handleInitialize))
	mux.HandleFunc("/api/logs", s.auth(s.handleLogs))
	mux.HandleFunc("/api/control/stop", s.auth(s.handleStop))
	mux.HandleFunc("/api/control/restart", s.auth(s.handleRestart))

	log.Info().Str("addr", addr).Msg("webui server starting")
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Error().Err(err).Msg("webui server error")
		}
	}()
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte(adminUser)) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(adminPass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="eth-perp-admin"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleParams(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]interface{}{
			"current":  s.params.Get(),
			"defaults": s.params.Defaults(),
		})
	case http.MethodPost:
		var snap LiveParamsSnapshot
		if err := json.NewDecoder(r.Body).Decode(&snap); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := validateSnapshot(snap); err != nil {
			http.Error(w, "validation failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		s.params.Update(snap)
		log.Info().Interface("params", snap).Msg("webui: params updated")
		writeJSON(w, map[string]interface{}{"status": "ok", "current": s.params.Get()})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.params.Reset()
	log.Info().Msg("webui: params reset to initialized defaults")
	writeJSON(w, map[string]interface{}{"status": "reset", "current": s.params.Get()})
}

func (s *Server) handleInitialize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defaults := s.params.SetDefaultsToCurrent()
	log.Info().Interface("params", defaults).Msg("webui: current params saved as initialized defaults")
	writeJSON(w, map[string]interface{}{
		"status":   "initialized",
		"defaults": defaults,
		"current":  s.params.Get(),
	})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]interface{}{
		"entries": s.events.Recent(),
	})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.handleControl(w, r, "stop")
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	s.handleControl(w, r, "restart")
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request, action string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	log.Warn().Str("action", action).Str("service", s.serviceName).Msg("webui: service control requested")
	writeJSON(w, map[string]string{"status": "accepted", "action": action})

	go func() {
		time.Sleep(500 * time.Millisecond)
		if err := exec.Command("systemctl", action, s.serviceName).Run(); err != nil {
			log.Error().Err(err).Str("action", action).Str("service", s.serviceName).Msg("systemctl command failed")
		}
	}()
}

func validateSnapshot(s LiveParamsSnapshot) error {
	if s.StopLossPct <= 0 || s.StopLossPct > 0.02 {
		return fmt.Errorf("stop_loss_pct must be in (0, 0.02]")
	}
	if s.TakeProfitPct <= 0 || s.TakeProfitPct > 0.05 {
		return fmt.Errorf("take_profit_pct must be in (0, 0.05]")
	}
	if s.TakeProfitPct/s.StopLossPct < 1.5 {
		return fmt.Errorf("take_profit/stop_loss ratio must be >= 1.5")
	}
	if s.CooldownAfterExitSec < 0 || s.MaxHoldingTimeSec <= 0 || s.MinHoldingTimeSec < 0 {
		return fmt.Errorf("holding and cooldown seconds must be valid positive values")
	}
	if s.MinHoldingTimeSec > s.MaxHoldingTimeSec {
		return fmt.Errorf("min_holding_time_sec cannot exceed max_holding_time_sec")
	}
	if s.LotSize == "" {
		return fmt.Errorf("lot_size is required")
	}
	if s.Leverage < 1 || s.Leverage > 150 {
		return fmt.Errorf("leverage must be in [1, 150]")
	}
	if s.MakerOffsetBps < 0 || s.MakerOffsetBps > 20 {
		return fmt.Errorf("maker_offset_bps must be in [0, 20]")
	}
	if s.GuardDeadlineMs < 100 || s.GuardDeadlineMs > 180 {
		return fmt.Errorf("guard_deadline_ms must be in [100, 180]")
	}
	if s.TrendConfidence < 0 || s.TrendConfidence > 1 ||
		s.SqueezeConfidence < 0 || s.SqueezeConfidence > 1 ||
		s.TransitionConfidence < 0 || s.TransitionConfidence > 1 {
		return fmt.Errorf("confidence values must be in [0, 1]")
	}
	if s.MaxSlippageBps <= 0 || s.MaxSlippageBps > 100 {
		return fmt.Errorf("max_slippage_bps must be in (0, 100]")
	}
	if s.DailyLossLimitPct <= 0 || s.DailyLossLimitPct > 1 {
		return fmt.Errorf("daily_loss_limit_pct must be in (0, 1]")
	}
	if s.ConsecutiveLossLimit <= 0 {
		return fmt.Errorf("consecutive_loss_limit must be > 0")
	}
	return nil
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(adminHTML))
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

const adminHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>ETH-Perp 控制台</title>
<style>
*{box-sizing:border-box}body{margin:0;font-family:Segoe UI,Arial,sans-serif;background:#101418;color:#e8edf2;padding:24px}.top{display:flex;justify-content:space-between;gap:16px;align-items:center;margin-bottom:18px}h1{font-size:22px;margin:0;color:#f2f6fa}.badge{font-size:12px;color:#79c0ff;border:1px solid #27547a;border-radius:999px;padding:4px 9px}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:14px}.card{background:#171d23;border:1px solid #29333d;border-radius:8px;padding:16px}.card h2{font-size:14px;margin:0 0 12px;color:#a9b7c4}.field{margin-bottom:12px}.field label{display:flex;justify-content:space-between;gap:12px;font-size:13px;color:#d6dee6;margin-bottom:6px}.field span{color:#79c0ff;font-family:Consolas,monospace;white-space:nowrap}input{width:100%;background:#0d1116;border:1px solid #34414d;border-radius:5px;color:#e8edf2;padding:7px 9px}input[type=range]{padding:0;accent-color:#2f81f7}.check{display:flex;align-items:center;gap:8px;font-size:13px;color:#d6dee6}.check input{width:auto}.actions{display:flex;flex-wrap:wrap;gap:10px;margin-top:18px}button{border:0;border-radius:6px;padding:9px 14px;color:#fff;font-weight:600;cursor:pointer}.save{background:#238636}.init{background:#8957e5}.reset{background:#3b434c}.restart{background:#0969da}.stop{background:#da3633}.msg{display:none;margin-top:12px;border-radius:6px;padding:9px 12px;font-size:13px}.ok{display:block;background:#102b1a;color:#56d364;border:1px solid #238636}.err{display:block;background:#341416;color:#ff7b72;border:1px solid #8e2b31}.panel{margin-top:18px}.logbar{display:flex;justify-content:space-between;align-items:center;gap:12px;margin-bottom:10px}.loglist{height:260px;overflow:auto;background:#0d1116;border:1px solid #29333d;border-radius:8px}.row{display:grid;grid-template-columns:170px 88px 1fr;gap:10px;padding:9px 12px;border-bottom:1px solid #202a33;font-size:13px}.row:last-child{border-bottom:0}.time{color:#8b949e}.type{font-family:Consolas,monospace;color:#79c0ff}.type.OPEN{color:#56d364}.type.CLOSE{color:#ffb86b}.details{color:#d6dee6;word-break:break-word}.muted{color:#8b949e;font-size:13px}@media(max-width:760px){body{padding:14px}.top{align-items:flex-start;flex-direction:column}.row{grid-template-columns:1fr}.time,.type{font-size:12px}}
</style>
</head>
<body>
<div class="top"><h1>ETH-Perp 控制台</h1><div class="badge">Basic Auth: vera@1030</div></div>
<form id="form">
<div class="grid" id="grid"></div>
<div class="actions">
  <button class="save" type="submit">保存参数</button>
  <button class="init" type="button" id="initBtn">初始化为当前参数</button>
  <button class="reset" type="button" id="resetBtn">恢复初始化值</button>
  <button class="restart" type="button" id="restartBtn">重启系统</button>
  <button class="stop" type="button" id="stopBtn">停止系统</button>
</div>
<div id="msg" class="msg"></div>
</form>
<section class="panel card">
  <div class="logbar"><h2>开单日志</h2><span class="muted" id="logHint">自动刷新</span></div>
  <div class="loglist" id="logs"></div>
</section>
<script>
const groups=[
{title:'止盈止损',items:[
['take_profit_pct','range','止盈比例',0.001,0.05,0.001,v=>(v*100).toFixed(2)+'%'],
['stop_loss_pct','range','止损比例',0.001,0.02,0.001,v=>(v*100).toFixed(2)+'%'],
['cooldown_after_exit_sec','number','平仓冷却秒',0,300,1,v=>v+'s'],
['max_holding_time_sec','number','最长持仓秒',60,86400,60,v=>v+'s'],
['min_holding_time_sec','number','最短持仓秒',0,3600,5,v=>v+'s']]},
{title:'交易执行',items:[
['lot_size','number','每笔手数',0.001,100,0.001,v=>String(v)],
['leverage','number','杠杆倍数',1,150,1,v=>v+'x'],
['use_maker_mode','checkbox','Maker 模式',0,0,0,v=>v?'开':'关'],
['maker_offset_bps','range','Maker 偏移 bps',0,20,0.1,v=>v.toFixed(1)+' bps'],
['guard_deadline_ms','number','保护单截止 ms',100,180,1,v=>v+'ms']]},
{title:'引擎权重/阈值',items:[
['trend_enabled','checkbox','趋势引擎',0,0,0,v=>v?'开':'关'],
['trend_confidence','range','趋势置信度',0.3,0.95,0.01,v=>v.toFixed(2)],
['oi_delta_threshold','range','OI Delta 阈值',0.001,0.02,0.001,v=>(v*100).toFixed(2)+'%'],
['squeeze_enabled','checkbox','Squeeze 引擎',0,0,0,v=>v?'开':'关'],
['squeeze_confidence','range','Squeeze 置信度',0.3,0.95,0.01,v=>v.toFixed(2)],
['basis_zscore_threshold','range','Basis ZScore',1,5,0.1,v=>v.toFixed(1)],
['transition_enabled','checkbox','Transition 引擎',0,0,0,v=>v?'开':'关'],
['transition_confidence','range','Transition 置信度',0.3,0.95,0.01,v=>v.toFixed(2)],
['vol_compression_ratio','range','波动压缩比例',0.1,0.9,0.05,v=>v.toFixed(2)]]},
{title:'风控',items:[
['max_slippage_bps','range','最大滑点 bps',1,100,0.5,v=>v.toFixed(1)+' bps'],
['daily_loss_limit_pct','range','日亏损限制',0.001,0.2,0.001,v=>(v*100).toFixed(2)+'%'],
['consecutive_loss_limit','number','连续亏损次数',1,20,1,v=>v],
['btc_correlation_lock_threshold','range','BTC 相关锁定阈值',0.1,1,0.01,v=>v.toFixed(2)]]}
];
const fields=[];
const grid=document.getElementById('grid');
for(const g of groups){const card=document.createElement('div');card.className='card';card.innerHTML='<h2>'+g.title+'</h2>';for(const it of g.items){fields.push(it);const[id,type,label,min,max,step]=it;const wrap=document.createElement('div');wrap.className=type==='checkbox'?'check field':'field';wrap.innerHTML=type==='checkbox'?'<input id="'+id+'" type="checkbox"><label for="'+id+'">'+label+' <span id="v_'+id+'"></span></label>':'<label for="'+id+'">'+label+' <span id="v_'+id+'"></span></label><input id="'+id+'" type="'+type+'" min="'+min+'" max="'+max+'" step="'+step+'">';card.appendChild(wrap)}grid.appendChild(card)}
function fmt(it,val){return it[6](it[1]==='checkbox'?!!val:parseFloat(val))}
function refreshLabel(it){const el=document.getElementById(it[0]);document.getElementById('v_'+it[0]).textContent=fmt(it,it[1]==='checkbox'?el.checked:el.value)}
function populate(cur){for(const it of fields){const el=document.getElementById(it[0]);if(cur[it[0]]===undefined)continue;if(it[1]==='checkbox')el.checked=!!cur[it[0]];else el.value=cur[it[0]];refreshLabel(it)}}
function collect(){const o={};for(const it of fields){const el=document.getElementById(it[0]);o[it[0]]=it[1]==='checkbox'?el.checked:(it[0]==='lot_size'?String(el.value):Number(el.value))}o.leverage=parseInt(o.leverage);o.cooldown_after_exit_sec=parseInt(o.cooldown_after_exit_sec);o.max_holding_time_sec=parseInt(o.max_holding_time_sec);o.min_holding_time_sec=parseInt(o.min_holding_time_sec);o.guard_deadline_ms=parseInt(o.guard_deadline_ms);o.consecutive_loss_limit=parseInt(o.consecutive_loss_limit);return o}
function msg(t,ok){const m=document.getElementById('msg');m.textContent=t;m.className='msg '+(ok?'ok':'err');setTimeout(()=>m.className='msg',4000)}
for(const it of fields){document.getElementById(it[0]).addEventListener('input',()=>refreshLabel(it))}
document.getElementById('form').addEventListener('submit',async e=>{e.preventDefault();const p=collect();if(p.take_profit_pct/p.stop_loss_pct<1.5){msg('盈亏比必须 >= 1.5',false);return}const r=await fetch('/api/params',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(p)});if(r.ok){const d=await r.json();populate(d.current);msg('参数已保存',true)}else msg('保存失败: '+await r.text(),false)});
document.getElementById('resetBtn').onclick=async()=>{if(!confirm('恢复到初始化参数？'))return;const r=await fetch('/api/params/reset',{method:'POST'});if(r.ok){const d=await r.json();populate(d.current);msg('已恢复初始化值',true)}else msg('恢复失败',false)};
document.getElementById('initBtn').onclick=async()=>{if(!confirm('把当前参数保存为新的初始化值？'))return;const r=await fetch('/api/params/initialize',{method:'POST'});if(r.ok){msg('当前参数已保存为初始化值',true)}else msg('初始化失败',false)};
document.getElementById('restartBtn').onclick=async()=>{if(!confirm('确认重启系统服务？'))return;const r=await fetch('/api/control/restart',{method:'POST'});msg(r.ok?'已发送重启命令':'重启命令失败',r.ok)};
document.getElementById('stopBtn').onclick=async()=>{if(!confirm('确认停止系统服务？停止后需要用 SSH 或服务器面板启动。'))return;const r=await fetch('/api/control/stop',{method:'POST'});msg(r.ok?'已发送停止命令':'停止命令失败',r.ok)};
function renderLogs(entries){const box=document.getElementById('logs');box.innerHTML='';if(!entries.length){box.innerHTML='<div class="row"><div class="details">暂无开单/平仓记录</div></div>';return}for(const e of entries.slice().reverse()){const f=e.fields||{};const detail=[e.message,f.dir&&('方向 '+f.dir),f.qty&&('数量 '+f.qty),f.entry&&('开仓 '+f.entry),f.exit&&('平仓 '+f.exit),f.pnl_pct!==undefined&&('PnL '+Number(f.pnl_pct).toFixed(4)+'%'),f.reason&&('原因 '+f.reason)].filter(Boolean).join(' | ');const row=document.createElement('div');row.className='row';row.innerHTML='<div class="time">'+new Date(e.time).toLocaleString()+'</div><div class="type '+e.type+'">'+e.type+'</div><div class="details">'+detail+'</div>';box.appendChild(row)}}
async function loadLogs(){try{const r=await fetch('/api/logs');const d=await r.json();renderLogs(d.entries||[]);document.getElementById('logHint').textContent='最后刷新 '+new Date().toLocaleTimeString()}catch(e){document.getElementById('logHint').textContent='日志加载失败'}}
(async()=>{try{const r=await fetch('/api/params');const d=await r.json();populate(d.current)}catch(e){msg('加载参数失败: '+e,false)}loadLogs();setInterval(loadLogs,3000)})();
</script>
</body>
</html>`
