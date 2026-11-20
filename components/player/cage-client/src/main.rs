// Relay one sandboxed browser's ordinary Wayland protocol to Cage. The host
// gets compositor feedback over an independent private socket. No artwork
// pixels travel through this process.
use std::{env, error::Error, os::fd::FromRawFd, os::unix::net::UnixDatagram,
          process::Command, rc::Rc, sync::Arc};
use wl_proxy::{
    baseline::Baseline, fixed::Fixed, global_mapper::GlobalMapper,
    object::{Object, ObjectCoreApi, ObjectRcUtils, ObjectUtils},
    protocols::{ObjectInterface,
        presentation_time::{
            wp_presentation::WpPresentation,
            wp_presentation_feedback::{WpPresentationFeedback, WpPresentationFeedbackHandler, WpPresentationFeedbackKind}},
        xdg_shell::{xdg_surface::{XdgSurface,XdgSurfaceHandler},xdg_toplevel::XdgToplevel,xdg_wm_base::{XdgWmBase,XdgWmBaseHandler}},
        wayland::{wl_display::{WlDisplay,WlDisplayHandler}, wl_registry::{WlRegistry,WlRegistryHandler},
                  wl_pointer::{WlPointer,WlPointerHandler,WlPointerButtonState}, wl_seat::{WlSeat,WlSeatHandler},
                  wl_surface::{WlSurface,WlSurfaceHandler}}},
    simple::{SimpleProxy,SimpleCommandExt},
};

struct Display(Arc<UnixDatagram>);
impl WlDisplayHandler for Display {
    fn handle_get_registry(&mut self, display: &Rc<WlDisplay>, registry: &Rc<WlRegistry>) {
        registry.set_handler(Registry { map: GlobalMapper::default(), host: self.0.clone(), presentation: None });
        display.send_get_registry(registry);
    }
}
struct Registry { map: GlobalMapper, host: Arc<UnixDatagram>, presentation: Option<Rc<WpPresentation>> }
impl WlRegistryHandler for Registry {
    fn handle_global(&mut self, registry: &Rc<WlRegistry>, name:u32, interface:ObjectInterface, version:u32) {
        if interface==ObjectInterface::WpPresentation {
            let presentation=registry.state().create_object::<WpPresentation>(version.min(2));
            registry.send_bind(name,presentation.clone());
            self.presentation=Some(presentation);
        }
        match interface.name() {
            "wl_compositor" | "wl_subcompositor" | "wl_output" | "wl_seat" | "wl_shm" |
            "xdg_wm_base" | "wp_presentation" | "wp_viewporter" | "wp_single_pixel_buffer_manager_v1" |
            "wp_fractional_scale_manager_v1" | "zwp_linux_dmabuf_v1" | "wp_linux_drm_syncobj_manager_v1" |
            "zxdg_output_manager_v1" | "zxdg_decoration_manager_v1" =>
                self.map.forward_global(registry,name,interface,version),
            _ => self.map.ignore_global(name),
        }
    }
    fn handle_global_remove(&mut self,registry:&Rc<WlRegistry>,name:u32) {
        self.map.forward_global_remove(registry,name);
    }
    fn handle_bind(&mut self,registry:&Rc<WlRegistry>,name:u32,id:Rc<dyn Object>) {
        if id.interface()==ObjectInterface::XdgWmBase {
            if let Some(presentation)=&self.presentation {
                id.downcast::<XdgWmBase>().set_handler(Shell(self.host.clone(),presentation.clone()));
            }
        }
        if id.interface()==ObjectInterface::WlSeat {
            id.downcast::<WlSeat>().set_handler(Seat(self.host.clone()));
        }
        self.map.forward_bind(registry,name,&id);
    }
}
struct Seat(Arc<UnixDatagram>);
impl WlSeatHandler for Seat {
    fn handle_get_pointer(&mut self, seat: &Rc<WlSeat>, pointer: &Rc<WlPointer>) {
        pointer.set_handler(Pointer { host: self.0.clone(), x: 0, y: 0 });
        seat.send_get_pointer(pointer);
    }
}
struct Pointer { host: Arc<UnixDatagram>, x: i32, y: i32 }
impl Pointer {
    fn report(&self, pressed: bool) {
        // Three native-endian words distinguish input from the 8-byte presentation timestamp.
        let mut packet = [0u8; 12];
        packet[..4].copy_from_slice(&i32::from(pressed).to_ne_bytes());
        packet[4..8].copy_from_slice(&self.x.to_ne_bytes());
        packet[8..].copy_from_slice(&self.y.to_ne_bytes());
        let _ = self.host.send(&packet);
    }
}
impl WlPointerHandler for Pointer {
    fn handle_enter(&mut self, p: &Rc<WlPointer>, serial: u32, surface: &Rc<WlSurface>, x: Fixed, y: Fixed) {
        self.x=x.to_f64() as i32; self.y=y.to_f64() as i32;
        p.send_enter(serial,surface,x,y);
        p.send_set_cursor(serial,None,0,0);
    }
    fn handle_motion(&mut self, p: &Rc<WlPointer>, time: u32, x: Fixed, y: Fixed) {
        self.x=x.to_f64() as i32; self.y=y.to_f64() as i32;
        p.send_motion(time,x,y); self.report(false);
    }
    fn handle_button(&mut self,p: &Rc<WlPointer>,serial:u32,time:u32,button:u32,state:WlPointerButtonState) {
        p.send_button(serial,time,button,state);
        self.report(state==WlPointerButtonState::PRESSED);
    }
    fn handle_set_cursor(&mut self,p: &Rc<WlPointer>,serial:u32,_surface:Option<&Rc<WlSurface>>,_x:i32,_y:i32) {
        p.send_set_cursor(serial,None,0,0);
    }
}
#[derive(Clone)]
struct Shell(Arc<UnixDatagram>,Rc<WpPresentation>);
impl XdgWmBaseHandler for Shell {
    fn handle_get_xdg_surface(&mut self,base:&Rc<XdgWmBase>,id:&Rc<XdgSurface>,surface:&Rc<WlSurface>) {
        id.set_handler(Role(self.clone(),surface.clone()));
        base.send_get_xdg_surface(id,surface);
    }
}
struct Role(Shell,Rc<WlSurface>);
impl XdgSurfaceHandler for Role {
    fn handle_get_toplevel(&mut self,surface:&Rc<XdgSurface>,id:&Rc<XdgToplevel>) {
        self.1.set_handler(self.0.clone());
        surface.send_get_toplevel(id);
    }
}
impl WlSurfaceHandler for Shell {
    fn handle_commit(&mut self,surface:&Rc<WlSurface>) {
        // Observe only the kiosk toplevel. Cursor/tooltip feedback cannot
        // acknowledge an artwork, even if Chromium commits them first.
        self.1.new_send_feedback(surface).set_handler(Feedback(self.0.clone()));
        surface.send_commit();
    }
}
struct Feedback(Arc<UnixDatagram>);
impl WpPresentationFeedbackHandler for Feedback {
    fn handle_presented(&mut self,_f:&Rc<WpPresentationFeedback>,hi:u32,lo:u32,ns:u32,_refresh:u32,
                        _sh:u32,_sl:u32,_flags:WpPresentationFeedbackKind){
        let timestamp=((u64::from(hi)<<32)|u64::from(lo))*1_000_000_000+u64::from(ns);
        let _=self.0.send(&timestamp.to_ne_bytes());
    }
}
fn main() -> Result<(),Box<dyn Error>> {
    // Duplicate with CLOEXEC and close FD 5 before starting the artwork process.
    let inherited=unsafe {UnixDatagram::from_raw_fd(5)};
    let host=Arc::new(inherited.try_clone()?);
    drop(inherited);
    host.set_nonblocking(true)?;
    let server=SimpleProxy::new(Baseline::ALL_OF_THEM)?;
    let mut args=env::args_os().skip(1);
    Command::new(args.next().ok_or("missing browser launcher")?).args(args)
        .with_wayland_display(server.display()).spawn_and_forward_exit_code()?;
    Err(Box::new(server.run(||Display(host.clone()))))
}
