"""Optional Playwright regression: real DOM, synthetic data, no upstream traffic."""
import argparse
import datetime
import json
import os
import pathlib
from playwright.sync_api import sync_playwright

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--browser", default=os.environ.get("BROWSER_EXECUTABLE"),
                    help="Use an installed Chromium-compatible browser instead of Playwright Chromium")
parser.add_argument("--baseline", action="store_true", help="Record observations without fix assertions")
args = parser.parse_args()
ROOT = pathlib.Path(__file__).resolve().parents[2]
(ROOT / "logs").mkdir(exist_ok=True)
TRACE = "polling-flicker-" + datetime.datetime.now().strftime("%Y%m%d-%H%M%S")


def audit(row):
    row.setdefault("at", datetime.datetime.now().astimezone().isoformat())
    row.setdefault("traceId", TRACE)
    with (ROOT / "logs" / (TRACE + ".jsonl")).open("a", encoding="utf8") as f:
        f.write(json.dumps(row, ensure_ascii=True) + "\n")
    print(json.dumps(row, ensure_ascii=True))


html = (ROOT / "web/index.html").read_text(encoding="utf8").replace(
    '<link rel="stylesheet" href="style.css">',
    "<style>" + (ROOT / "web/style.css").read_text(encoding="utf8") + "</style>"
).replace('<script src="app.js"></script>', "")
source = (ROOT / "web/app.js").read_text(encoding="utf8").rsplit(
    '  if (document.readyState === "loading")', 1)[0] + """
window.review={state,api,renderStatus,renderAccounts,renderModels,renderStats,loadStats,
  showAlert,hideAlert,togglePolicy};})();"""

with sync_playwright() as pw:
    try:
        browser = pw.chromium.launch(executable_path=args.browser, headless=True)
    except Exception as error:
        audit({"traceId": TRACE, "event": "browser_launch_failed", "error_type": type(error).__name__})
        raise
    try:
        context = browser.new_context(viewport={"width": 1920, "height": 1100})
        context.route("**/*", lambda route: route.fulfill(
            status=200, content_type="text/html", body=html))
        page = context.new_page()
        errors = []
        page.on("pageerror", lambda e: errors.append(str(e)))
        page.goto("http://polling-review.test/")
        page.evaluate(source)
        page.evaluate("""()=>{
          document.documentElement.setAttribute('data-theme','dark');
          document.getElementById('dashboard').classList.remove('hidden');
          document.getElementById('tabs').classList.remove('hidden');
          review.state.apiKey='synthetic-admin-key';
          window.accounts=[
            {path:'synthetic-a.json',nickname:'测试甲',edition:'cn',state:'active',
              schedulingEnabled:true,quotaKnown:true,quotaTotal:100,quotaUsed:1,quotaRemaining:99},
            {path:'synthetic-b.json',nickname:'测试乙',edition:'intl',state:'active',
              schedulingEnabled:true,quotaKnown:true,quotaTotal:200,quotaUsed:2,quotaRemaining:198}];
          review.renderAccounts(accounts);
          const start=1793000000000;
          window.stats={start:'2026-10-26',end:'2026-10-28',serverTime:start+1000,
            ttftMetric:'first_token_v2',summary:{requests:2,success:2,failed:0,tokens:300,
              inputTokens:200,outputTokens:100,cachedTokens:0,tokenSamples:2,
              credits:1,creditSamples:2,avgLatencyMs:1200,avgTtftMs:300,ttftSamples:2},
            buckets:[{start,label:'已到时间',requests:2,tokens:300,tokenSamples:2,
              credits:1,creditSamples:2,avgLatencyMs:1200,avgTtftMs:300},
              {start:start+3600000,label:'未来时间',requests:0}],
            models:[],modelLatency:[],accounts:[],storage:{mode:'file'}};
          review.renderStats(stats);
          window.fetch=async()=>({ok:true,status:200,json:async()=>({})});
        }""")
        results = page.evaluate("""async()=>{
          const top=()=>document.getElementById('dashboard').getBoundingClientRect().top;
          review.hideAlert(); const before=top();
          review.showAlert('合成账号启停成功',true);
          const style=getComputedStyle(document.getElementById('alert'));
          const color=style.color.match(/\\d+/g).map(Number);
          const shift=top()-before;
          await review.api('/admin/api/status');
          const successLost=document.getElementById('alert').classList.contains('hidden');
          review.showAlert('合成开关失败',false,'/admin/api/account-toggle');
          await review.api('/admin/api/status');
          const errorLost=document.getElementById('alert').classList.contains('hidden');
          review.hideAlert();
          const tb=document.querySelector('#accountsTable tbody');
          const first=tb.rows[0],second=tb.rows[1],button=first.querySelector('.policy-switch');
          review.renderAccounts(accounts);
          const sameRow=tb.rows[0]===first;
          accounts[0].quotaUsed=3;accounts[0].quotaRemaining=97;
          review.renderAccounts(accounts);
          const otherPreserved=tb.rows[1]===second;
          const buttonPreserved=tb.rows[0].querySelector('.policy-switch')===button;
          const svg=document.querySelector('#usageChart svg');
          review.renderStats({...stats,serverTime:stats.serverTime+1000});
          const chartPreserved=svg===document.querySelector('#usageChart svg');
          let finish;
          window.fetch=async path=>path.startsWith('/admin/api/stats?')?
            new Promise(resolve=>{finish=()=>resolve({ok:true,status:200,json:async()=>stats})}):
            ({ok:true,status:200,json:async()=>({})});
          const hintBefore=document.getElementById('statsInfo').textContent;
          const loading=review.loadStats(false);
          const hintDuring=document.getElementById('statsInfo').textContent;
          finish();await loading;
          return {success_is_red:color[0]>color[1],notification_layout_shift:shift,
            success_lost_on_poll:successLost,error_lost_on_unrelated_poll:errorLost,
            identical_account_row_preserved:sameRow,unaffected_account_row_preserved:otherPreserved,
            unaffected_switch_preserved:buttonPreserved,identical_stats_chart_preserved:chartPreserved,
            background_stats_hint_changed:hintBefore!==hintDuring};
        }""")
        if not args.baseline:
            more = page.evaluate("""async()=>{
              review.state.activeTab='accounts';
              document.getElementById('panel-overview').classList.remove('active');
              document.getElementById('panel-accounts').classList.add('active');
              let fail=false;const calls=[];
              window.fetch=async(path,options={})=>{
                calls.push({path,event:path==='/admin/api/client-events'?
                  JSON.parse(options.body).event:undefined});
                if(path==='/admin/api/account-toggle'){
                  if(fail)return {ok:false,status:409,json:async()=>({error:'合成冲突，请重试'})};
                  accounts[0].schedulingEnabled=JSON.parse(options.body).enabled;
                  return {ok:true,status:200,json:async()=>({saved:true})};
                }
                return {ok:true,status:200,json:async()=>({accounts,
                  gateway:{version:'synthetic',webuiEnabled:true},health:{percent:100,state:'healthy'}})};
              };
              review.renderAccounts(accounts);
              const tb=document.querySelector('#accountsTable tbody'),row=tb.rows[0],
                other=tb.rows[1],otherButton=other.querySelector('.policy-switch');
              await review.togglePolicy({target:row.querySelector('.policy-switch')});
              await new Promise(r=>setTimeout(r,20));
              const successVisible=!document.getElementById('alert').classList.contains('hidden') &&
                document.getElementById('alert').classList.contains('ok');
              const toggled=tb.rows[0].querySelector('.policy-switch').getAttribute('aria-checked')==='false';
              fail=true;
              await review.togglePolicy({target:tb.rows[0].querySelector('.policy-switch')});
              await new Promise(r=>setTimeout(r,20));
              const failureVisible=!document.getElementById('alert').classList.contains('hidden') &&
                document.getElementById('alert').textContent.includes('409');
              const failedDidNotFlip=tb.rows[0].querySelector('.policy-switch').getAttribute('aria-checked')==='false';
              otherButton.focus();
              for(let i=0;i<50;i++){accounts[0].quotaUsed=i;review.renderAccounts(accounts);}
              const stable=tb.rows[0]===row&&tb.rows[1]===other&&
                tb.rows[1].querySelector('.policy-switch')===otherButton;
              const focusPreserved=document.activeElement===otherButton;
              review.renderStats(stats);
              const option=document.querySelector('#statsAccount option');
              review.renderStats({...stats,summary:{...stats.summary,tokens:900}});
              const optionPreserved=option===document.querySelector('#statsAccount option');
              const modelData={models:[{id:'null',enabled:true,requests:1,cnMultiplier:'1.00x'},
                {id:'__proto__',enabled:true,requests:2}]};
              review.renderModels(modelData);
              const mt=document.querySelector('#modelsTable tbody');
              const modelRow=mt.rows[0],modelOther=mt.rows[1];
              const detailButton=modelRow.querySelector('.rate-info');
              const panel=modelRow.querySelector('.rate-detail');
              detailButton.setAttribute('aria-expanded','true');panel.classList.remove('hidden');
              panel.textContent='合成展开内容';review.state.openRateKeys['cn|null']=true;
              modelData.models[0].requests=3;review.renderModels(modelData);
              const detailPreserved=panel===mt.rows[0].querySelector('.rate-detail')&&
                panel.textContent==='合成展开内容';
              review.renderModels({models:modelData.models.slice().reverse()});
              const reordered=mt.rows[0]===modelOther&&mt.rows[1]===modelRow;
              review.renderModels({models:[modelData.models[0]]});
              const removed=mt.rows.length===1&&mt.rows[0]===modelRow;
              return {toggle_success_readable:successVisible,toggle_applied:toggled,
                toggle_failure_not_hidden:failureVisible,failed_toggle_not_faked:failedDidNotFlip,
                fifty_updates_preserve_unaffected_nodes:stable,
                background_focus_preserved:focusPreserved,unchanged_stats_options_preserved:optionPreserved,
                expanded_model_details_preserved:detailPreserved,keyed_reorder_preserved:reordered,
                keyed_removal_correct:removed,request_failures_audited:
                  calls.some(c=>c.event==='request_failed')};
            }""")
            assert all(more.values()), more
            results.update(more)
            themes = page.evaluate("""()=>{
              const results=[];
              for(const theme of ['light','dark']){
                document.documentElement.setAttribute('data-theme',theme);
                review.showAlert('合成成功提示',true);
                let color=getComputedStyle(document.getElementById('alert')).color.match(/\\d+/g).map(Number);
                results.push(color[1]>color[0]);
                review.showAlert('合成错误提示',false);
                color=getComputedStyle(document.getElementById('alert')).color.match(/\\d+/g).map(Number);
                results.push(color[0]>color[1]);
              }
              return results.every(Boolean);
            }""")
            assert themes, "Success/error theme colors must remain distinct"
            page.set_viewport_size({"width": 390, "height": 900})
            mobile = page.evaluate("""()=>{
              const top=document.getElementById('dashboard').getBoundingClientRect().top;
              review.showAlert('合成成功提示'.repeat(50),true);
              const r=document.getElementById('alert').getBoundingClientRect();
              return r.left>=0 && r.right<=innerWidth &&
                document.getElementById('dashboard').getBoundingClientRect().top===top;
            }""")
            assert mobile, "Mobile toast must fit the viewport without moving content"
            results.update(theme_colors_distinct=True, mobile_toast_stable=True)
        if not args.baseline:
            assert not results["success_is_red"], results
            assert results["notification_layout_shift"] == 0, results
            assert not results["success_lost_on_poll"] and not results["error_lost_on_unrelated_poll"], results
            assert results["identical_account_row_preserved"], results
            assert results["unaffected_account_row_preserved"] and results["unaffected_switch_preserved"], results
            assert results["identical_stats_chart_preserved"], results
            assert not results["background_stats_hint_changed"], results
        row = {"at": datetime.datetime.now().astimezone().isoformat(), "traceId": TRACE,
               "event": "flicker_baseline" if args.baseline else "flicker_fix_acceptance",
               **results, "page_errors": errors, "production_mutations": 0, "upstream_requests": 0}
        audit(row)
        assert not errors, errors
        context.close()
    except Exception as error:
        audit({"traceId": TRACE, "event": "browser_acceptance_failed",
               "error_type": type(error).__name__, "production_mutations": 0})
        raise
    finally:
        browser.close()
