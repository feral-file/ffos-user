// Relay one sandboxed browser's ordinary Wayland protocol to Cage. The host
// gets compositor feedback over an independent private socket. No artwork
// pixels travel through this process.
use std::{env, error::Error, os::fd::FromRawFd, os::unix::net::UnixDatagram,
          process::Command, rc::Rc, sync::Arc};
use wl_proxy::{
    baseline::Baseline, global_mapper::GlobalMapper,
    object::{Object, ObjectRcUtils, ObjectUtils},
    protocols::{ObjectInterface,
        presentation_time::{
            wp_presentation::{WpPresentation, WpPresentationHandler},
            wp_presentation_feedback::{WpPresentationFeedback, WpPresentationFeedbackHandler, WpPresentationFeedbackKind}},
        wayland::{wl_display::{WlDisplay,WlDisplayHandler}, wl_registry::{WlRegistry,WlRegistryHandler},
                  wl_surface::WlSurface}},
    simple::{SimpleProxy,SimpleCommandExt},
};

struct Display(Arc<UnixDatagram>);
impl WlDisplayHandler for Display {
    fn handle_get_registry(&mut self, display: &Rc<WlDisplay>, registry: &Rc<WlRegistry>) {
        registry.set_handler(Registry { map: GlobalMapper::default(), host: self.0.clone() });
        display.send_get_registry(registry);
    }
}
struct Registry { map: GlobalMapper, host: Arc<UnixDatagram> }
impl WlRegistryHandler for Registry {
    fn handle_global(&mut self, registry: &Rc<WlRegistry>, name:u32, interface:ObjectInterface, version:u32) {
        match interface.name() {
            "wl_compositor" | "wl_subcompositor" | "wl_output" | "wl_seat" | "wl_shm" |
            "xdg_wm_base" | "wp_presentation" | "wp_viewporter" | "wp_single_pixel_buffer_manager_v1" |
            "wp_fractional_scale_manager_v1" | "zwp_linux_dmabuf_v1" | "wp_linux_drm_syncobj_manager_v1" |
            "zxdg_output_manager_v1" | "zxdg_decoration_manager_v1" | "wp_cursor_shape_manager_v1" =>
                self.map.forward_global(registry,name,interface,version),
            _ => self.map.ignore_global(name),
        }
    }
    fn handle_global_remove(&mut self,registry:&Rc<WlRegistry>,name:u32) {
        self.map.forward_global_remove(registry,name);
    }
    fn handle_bind(&mut self,registry:&Rc<WlRegistry>,name:u32,id:Rc<dyn Object>) {
        if id.interface()==ObjectInterface::WpPresentation {
            id.downcast::<WpPresentation>().set_handler(Presentation(self.host.clone()));
        }
        self.map.forward_bind(registry,name,&id);
    }
}
struct Presentation(Arc<UnixDatagram>);
impl WpPresentationHandler for Presentation {
    fn handle_feedback(&mut self,p:&Rc<WpPresentation>,surface:&Rc<WlSurface>,feedback:&Rc<WpPresentationFeedback>){
        feedback.set_handler(Feedback(self.0.clone()));
        p.send_feedback(surface,feedback);
    }
}
struct Feedback(Arc<UnixDatagram>);
impl WpPresentationFeedbackHandler for Feedback {
    fn handle_presented(&mut self,f:&Rc<WpPresentationFeedback>,hi:u32,lo:u32,ns:u32,refresh:u32,
                        sh:u32,sl:u32,flags:WpPresentationFeedbackKind){
        let timestamp=((u64::from(hi)<<32)|u64::from(lo))*1_000_000_000+u64::from(ns);
        let _=self.0.send(&timestamp.to_ne_bytes());
        f.send_presented(hi,lo,ns,refresh,sh,sl,flags);
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
