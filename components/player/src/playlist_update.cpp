#include "ff/playback.hpp"
#include <algorithm>
#include <numeric>
#include <random>

namespace ff {
namespace {
// Match duplicate slots by ordinal within (id, source), never by URL alone.
// ID-only fallback is only for source replacement / fresh policy labels.
int match_slot(const Json& old_items, int old_index, const Json& items, bool source = true) {
  if(old_index<0||old_index>=static_cast<int>(old_items.size()))return -1;
  const auto& item=old_items[old_index];
  const auto same=[&](const Json& candidate){
    if(candidate.value("id","")!=item.value("id",""))return false;
    return source?candidate["source"]==item["source"]:!item.value("id","").empty();
  };
  int ordinal=0;
  for(int i=0;i<old_index;++i)if(same(old_items[i]))++ordinal;
  for(size_t i=0;i<items.size();++i)if(same(items[i])&&ordinal--==0)return static_cast<int>(i);
  return -1;
}
}

bool Playback::refresh_blocks(const Occurrence& occurrence, const Json& items, const Json& policy) const {
  const auto& old = occurrence.playlist["items"];
  int index = match_slot(old, occurrence.index, items);
  if (index < 0) index = match_slot(old, occurrence.index, items, false);
  return !allowed(index < 0 ? occurrence.item : items[index], policy);
}

void Playback::refresh_boot(Json& next, const Json& old_playlist, const Json& refreshed,
                            const std::string& context, const Json& plan) {
  auto& saved = next["boot"];
  if (saved.is_null()) return;
  const auto& previous = saved["playlist"];
  if (previous != old_playlist && (old_playlist.value("id","").empty() ||
      old_playlist["id"] != previous.value("id",""))) return;
  if (saved.contains("index")) {
    const int index = saved["index"].get<int>();
    int mapped = match_slot(previous["items"], index, refreshed["items"]);
    if (mapped < 0) mapped = match_slot(previous["items"], index, refreshed["items"], false);
    saved["index"] = std::max(mapped, 0);
  }
  saved["playlist"] = refreshed;
  saved["context"] = context;
  if (plan.is_null()) saved.erase("activationTimes");
  else saved["activationTimes"] = plan["activationTimes"];
}

void Playback::update_order(Json& next,int index) const {
  std::vector<int> order(next["playlist"]["items"].size());
  std::iota(order.begin(),order.end(),0);
  if(next["shuffle"]==true&&!order.empty()){
    std::iter_swap(order.begin(),order.begin()+index);
    std::mt19937 random(std::random_device{}());
    std::shuffle(order.begin()+1,order.end(),random);
  }
  next["index"]=index;next["order"]=order;
}

Json Playback::update_playlist(const Json& p,const Json& plan) {
  fields(p,{"items","context","retireBlockedCurrent"});
  require(!state_["playlist"].is_null(),"invalid_params","No playlist to update");
  require(p.contains("items"),"invalid_params","Missing items");
  if(p.contains("retireBlockedCurrent"))require(p["retireBlockedCurrent"].is_boolean(),"invalid_params","Expected retirement flag");
  auto playlist=state_["playlist"];playlist["items"]=p["items"];
  validate_playlist(playlist);
  auto context=p.value("context",state_["context"].get<std::string>());
  require(context=="curated"||context=="personal","invalid_params","Invalid content context");
  // A source refresh cannot widen a curated cast's trust domain.
  require(state_["context"]!="curated"||context!="personal","invalid_params","Refresh cannot reclassify curated playback as personal");
  const auto& old_items=state_["playlist"]["items"];
  const auto& items=playlist["items"];
  const bool unchanged=old_items==items&&state_["context"]==context;
  if(!unchanged){playlist.erase("signature");playlist.erase("signatures");}
  auto next=state_;next["context"]=context;
 const bool plan_changed=state_.value("cohortPlan",Json(nullptr))!=plan;
 next["cohortPlan"]=plan;
  const bool retire_selected=selected_&&refresh_blocks(*selected_,items,next);
  // An outgoing occurrence from a different cast keeps its own context and
  // labels; this refresh has no authority to relabel that other document.
  const bool same_presented=presented_&&presented_->cast_id==cast_id_;
  const bool retire_presented=same_presented&&refresh_blocks(*presented_,items,next);
  const bool retire=p.value("retireBlockedCurrent",false)||retire_selected||retire_presented;
  bool any=items.empty();for(const auto& item:items)any|=allowed(item,next);
  // When fresh labels block the current work, retirement must not be undone
  // by an all-blocked replacement. Retain that intent for future policy relax.
  require(any||retire,"content_blocked","No refreshed item is allowed; current playback retained");
  int index=match_slot(old_items,state_["index"].get<int>(),items);
  const bool keep=index>=0&&selected_&&!retire&&state_["context"]==context;
  const bool defer=!items.empty()&&selected_&&!retire&&index<0&&
    match_slot(old_items,state_["index"].get<int>(),items,false)<0&&state_["context"]==context;
  auto id=uuid();
  next["pendingUpdate"]=nullptr;
  next["updateOutcome"]={{"id",id},{"outcome",defer?"deferred":"applied"},{"contentBlocked",!any}};
  refresh_boot(next, plan.is_null() ? state_["playlist"] : state_["cohortPlan"]["playlist"],
               plan.is_null() ? playlist : plan["playlist"], context, plan);
  if(defer){
    next["pendingUpdate"]={{"id",id},{"playlist",playlist},{"context",context}};
  }else{
    next["playlist"]=playlist;
    if(index<0)index=match_slot(old_items,state_["index"].get<int>(),items,false);
    if(index<0)index=0;
    if(!items.empty()&&!allowed(items[index],next)){
      for(size_t n=0;n<items.size();++n){auto at=(index+n)%items.size();if(allowed(items[at],next)){index=static_cast<int>(at);break;}}
    }
    if(!unchanged)update_order(next,index);
    if(items.empty())next["stopped"]=true;
  }
  persist(std::move(next));
  // Identical refreshes cancel a deferred update without remounting, changing
  // mode/order or invalidating current trust bindings.
  if(!unchanged||plan_changed){
    ++generation_;
    // Failed slots are indices in the previous ordering. A refreshed playlist
    // can replace a broken work or append its first playable successor.
    exhausted_items_.clear();
  }
  if(retire_presented||p.value("retireBlockedCurrent",false)){release_input();presented_.reset();}
  auto amend=[&](Occurrence& o){
    int mapped=match_slot(o.playlist["items"],o.index,items);
    if(mapped<0)return;
    o.index=mapped;o.item=items[mapped];o.playlist=playlist;o.generation=generation_;o.context=context;
    refresh_display(o);
  };
  if(!defer){
    if(keep){amend(*selected_);if(same_presented&&presented_)amend(*presented_);}
    else select();
  }
  return {{"outcome",defer||(!keep&&selected_)?"accepted":"applied"},{"generation",generation_},
    {"revision",revision_},{"occurrence",selected_?Json(selected_->id):Json(nullptr)},{"updateId",id}};
}

bool Playback::apply_pending_update(){
  const auto pending=state_.value("pendingUpdate",Json(nullptr));
  if(pending.is_null())return false;
  auto next=state_;next["playlist"]=pending["playlist"];next["context"]=pending["context"];
  next["pendingUpdate"]=nullptr;
  next["updateOutcome"]={{"id",pending["id"]},{"outcome","applied"},{"contentBlocked",false}};
  const auto& items=next["playlist"]["items"];
  bool any=items.empty();int index=0;
  for(size_t n=0;n<items.size();++n)if(allowed(items[n],next)){any=true;index=static_cast<int>(n);break;}
  if(!any){
    // A later policy change can invalidate the queued projection. Expose that
    // terminal result; never repeatedly try the same invalid update each tick.
    next=state_;next["pendingUpdate"]=nullptr;
    next["updateOutcome"]={{"id",pending["id"]},{"outcome","failed"},{"contentBlocked",true}};
    persist(std::move(next));return false;
  }
  update_order(next,index);if(items.empty())next["stopped"]=true;
  persist(std::move(next));++generation_;select();return true;
}
}
