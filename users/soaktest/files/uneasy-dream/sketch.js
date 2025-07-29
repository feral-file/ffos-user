let seed;
let scene = 2;
let scene01, scene02, scene03;

let time = 0.0, ntime = 0.0, ptime = 0.0;
let totalTime = 32;
let sceneTime = 0;

let render, post;
let osc, amp, vel, nosc, namp, nvel;

let colors = ["#DF756A", "#83919B", "#B5A290", "#D1AD81", "#313A41", "#021166"];
function preload(){
  	post = loadShader('shaders/post.vert', 'shaders/post.frag');
}
function setup() {
	createCanvas(windowWidth, windowHeight, WEBGL);
	seed = random(9999999);
	smooth(0);
	noiseDetail(2);
	noCursor();

	gl = this._renderer.GL;
	gl.disable(gl.DEPTH_TEST);
	render = createGraphics(width, height, WEBGL);
	render.smooth(0);
	render._renderer.GL.disable(gl.DEPTH_TEST);
	render.background(140);

	osc = amp = vel = nosc = namp = nvel = 0;

	generate(scene);
}

function draw() {

	time = millis()*0.001;
	sceneTime += time-ptime;
	ptime= time;
	if(sceneTime >= totalTime){
		sceneTime = sceneTime%totalTime;
		let nextScene = int(random(3));
		while(nextScene == scene) nextScene = int(random(3))
		generate(nextScene);
	}
	ntime = sceneTime/totalTime;

	translate(-width*0.5, -height*0.5);

	if(int(scene) == 0) scene01.update();
	else if(int(scene) == 1) scene02.update();
	else if(int(scene) == 2) scene03.update();
	
	shader(post);
	post.setUniform('tex0', render);
	post.setUniform('time', time);

	let transShader = easeInOutQuad(constrain(ntime, 0, 1));
	transShader = pow(transShader, 1.18);
	post.setUniform('osc', lerp(osc, nosc, transShader));
	post.setUniform('vel', lerp(vel, nvel, transShader));
	post.setUniform('amp', lerp(amp, namp, transShader));

	rect(0,0,width, height);
	resetShader();
}

function windowResized(){
	resizeCanvas(windowWidth, windowHeight, {});
	render.resizeCanvas(windowWidth, windowHeight, {});
	render.background(140);
}

function generate(nscene){

	seed = random(9999999);
	randomSeed(seed);
	noiseSeed(seed);

	scene = nscene;

	if(scene == 0) scene01 = new Scene01();
	if(scene == 1) scene02 = new Scene02();
	if(scene == 2) scene03 = new Scene03();

	osc = nosc;
	vel = nvel;
	amp = namp;

	nosc = 20.3*random(0.1, 1.4*random(1))*random(1);
	nvel = 0.004*random(0.2, 1);
	namp = 0.02*random(0.2, 1)*random(1);

}

function rcol(){
	return colors[int(random(colors.length))];
}
function getColor() {
	return getColor(random(colors.length));
}
function getColor(v) {
	v = abs(v);
	v = v%(colors.length);
	let c1 = colors[int(v%colors.length)];
	let c2 = colors[int((v+1)%colors.length)];
	return lerpColor(color(c1), color(c2), pow(v%1, 1.6));
}

function easeInOutQuad(t){
	return t<0.5 ? 2*t*t : -1+(4-2*t)*t;
}

function easeInOutCubic(t){
	return t<0.5 ? 4*t*t*t : (t-1)*(2*t-2)*(2*t-2)+1;
}

function keyPressed(){
	saveCanvas();
}

//Scenes 01
class Cell {
	constructor(x, y) {
		this.x = x;
		this.y = y;
		this.remove = false;
		this.s = max(width, height)*random(0.025, 0.03)*random(1, random(1.2));
		this.col = rcol();
		this.tt = 0;
		this.life = random(4, 5);
		this.str = random(0.4, 1);
		this.rx = 0.0;
		this.ry = 0.0;
	}

	update() {
		this.tt += 1./60;

		this.x += this.rx;
		this.y += this.ry;
		this.rx = this.ry = 0;

		if (random(1) < 0.0005) this.remove = true;
		if(this.tt > this.life) this.remove = true;
	}

	repulsion(o) {
		let size = (this.s+o.s)*0.5;
		let md = pow(noise(des+this.x*det, des+this.y*det, time*0.2), 0.8)*size*0.8;
		if (abs(o.x-this.x) > md || abs(o.y-this.y) > md) return;
		let dis = dist(this.x, this.y, o.x, o.y)/size;
		if (dis > md) return;
		let ang = atan2(o.y-this.y, o.x-this.x)+random(-0.3, 0.3);
		let vel = constrain((dis-md)*1./md, -1, 1);
		let sign = (vel < 0)? -1 : 1;
		vel = pow(abs(vel), 5)*sign*0.005;//velRep;
		//vel *= deltaTime/60.0;
		vel *= velRep;

		this.rx += cos(ang)*vel;
		this.ry += sin(ang)*vel;

		o.rx += cos(ang+PI)*vel;
		o.ry += sin(ang+PI)*vel;
	}
}

let det, des;
let velRep = 1;
class Scene01{
	constructor(){
		this.count = int(random(50, 130));
		this.ampScale = random(0.22, 0.5);//0.3;//random(random(0.13, 0.44), 0.68);
		this.cells = [];
		this.angles = [];
		for (let i = 0; i < 2; i++) {
			let c = new Cell(width*random(-0.05, 0.05), height*random(-0.05, 0.05));
			this.cells.push(c);
		}
		this.cam = createVector();
		this.limX1 = 0; 
		this.limY1 = 0;
		this.limX2 = 0;
		this.limY2 = 0;
		this.sca = 1.0;
		this.alpha = random(4);
		det = random(0.01);
		des = random(1000);
		velRep = random(0.8, 1.2)*random(0.7, 1.3);
	}
	update(){


		let init = pow(constrain(ntime*12, 0, 1), 0.8);
		let end = 1-pow(constrain(ntime*10-9, 0, 1), 0.6);
		//let firstInit = constrain(time*0.2, 0, 1);

		let fade = init*end;
		
		if (this.cells.length < this.count) {
			let ind = int(random(1, this.cells.length));
			let p1 = this.cells[ind-1];
			let p2 = this.cells[ind+0];
			let cx = (p1.x+p2.x)*0.5;
			let cy = (p1.y+p2.y)*0.5;
			this.cells.splice(ind, 0, new Cell(cx, cy));
		}

		this.updateRepulsion();

		let center = createVector();
		for (let i = 0; i < this.cells.length; i++) {
			let c = this.cells[i];
			c.update();
			center.add(c.x, c.y);

			if (c.x < this.limX1 || i == 0) this.limX1 = c.x;
			if (c.y < this.limY1 || i == 0) this.limY1 = c.y;
			if (c.x > this.limX2 || i == 0) this.limX2 = c.x;
			if (c.y > this.limY2 || i == 0) this.limY2 = c.y;

			if (c.remove) {
				this.angles.splice(i, 1);
				this.cells.splice(i, 1);
				i--;
			}
		}

		center.div(this.cells.length);

		this.cam.lerp(center.copy().add(-(this.limX1+this.limX2), -(this.limY1+this.limY2)), 0.1);

		let ww = this.limX2-this.limX1;
		let hh = this.limY2-this.limY1;

		let nsca = (width*1.2/ww+height*1.2/hh)*this.ampScale;
		nsca = min(200, nsca);
		this.sca = lerp(this.sca, nsca, 0.6);

		render.push();
		render.scale(this.sca);//min(12, sca));
		render.translate(this.cam.x, this.cam.y);
		render.rectMode(CORNERS);

		let vc = time*0.2;
		let col = getColor(vc);
		render.fill(red(col), green(col), blue(col), this.alpha*fade);
		render.noStroke();
		render.beginShape();
		for (let i = 0; i < this.cells.length; i++) {
			let c = this.cells[i];
			render.vertex(c.x, c.y);
		}
		render.endShape();

		render.noStroke();
		let x1, y1, x2, y2;
		for (let i = 0; i < this.cells.length; i++) {
			let c = this.cells[i];
			let amp = map(cos(time*0.02+i+0.2), -1, 1, 2, 3)*constrain(c.tt*2.2, 0, 1)*0.9;
			amp = c.s*0.05*fade;
			let movAng = random(-0.2, 0.2)*random(1);
			let ang = this.angles[i]+movAng;
			let str = 0.6*c.str*fade;
			x1 = c.x+cos(ang+PI)*amp;
			y1 = c.y+sin(ang+PI)*amp;
			x2 = c.x+cos(ang)*amp;
			y2 = c.y+sin(ang)*amp;

			render.fill(red(c.col), green(c.col), blue(c.col), random(8, 10)*fade);
			
			render.beginShape();
			render.vertex(x1+cos(ang-HALF_PI)*str, y1+sin(ang-HALF_PI)*str);
			render.vertex(x1+cos(ang+HALF_PI)*str, y1+sin(ang+HALF_PI)*str);
			render.vertex(x2+cos(ang+HALF_PI)*str, y2+sin(ang+HALF_PI)*str);
			render.vertex(x2+cos(ang-HALF_PI)*str, y2+sin(ang-HALF_PI)*str);
			render.endShape();
			
		}
		render.pop();

	}

	updateRepulsion(){

		if(this.cells.length < 2) return;

		this.angles = [];
		let ant, act, nex;
		let cl = this.cells.length;
		let a1, a2, ang, cx, cy;
		for (let i = 0; i < cl; i++) {
			ant = this.cells[(i-1+cl)%cl];
			act = this.cells[(i+0+cl)%cl];
			nex = this.cells[(i+1+cl)%cl];

			a1 = atan2(nex.y-ant.y, nex.x-ant.x);
			a2 = atan2(act.y-ant.y, act.x-ant.x);
			ang = (a1+a2)*0.5-HALF_PI;
			this.angles.push(ang+map(cos(time*0.02+i), -1, 1, 0, 0.1));

			cx = (ant.x+nex.x)*0.5;
			cy = (ant.y+nex.y)*0.5;

			act.x = lerp(act.x, nex.x, 0.005);
			act.y = lerp(act.y, nex.y, 0.005);

			act.x += constrain((cx-act.x)*0.4, -5, 5);
			act.y += constrain((cy-act.y)*0.4, -5, 5);
		}

		for (let k = 0; k < this.cells.length; k++) {
			let c = this.cells[k];
			for (let j = k+1; j < this.cells.length; j++) {
				let o = this.cells[j];
				c.repulsion(o);
			}
		}
	}
}

////////////////////////////////////////////////////////////////////...
//Scene 02
class Bars{
	constructor(){
		this.countBars = int(random(1, random(5)));
		this.ws = [];
		this.velMovs = [];
		this.desMovs = [];
		this.cols = [];
		for(let i = 0; i < this.countBars; i++){
			this.ws[i] = random(6, random(20, 60));
			this.velMovs[i] = random(0.015)*random(0.4, 1);
			this.desMovs[i] = random(200)
			this.cols[i] = rcol();
		}
	}
	update(){
		let init = pow(constrain(ntime*7, 0, 1), 0.6);
		let end = 1-pow(constrain(ntime*10-9, 0, 1), 0.8);
		let ss = width*1.0/(this.countBars+1);
		render.noFill();
		render.noStroke();
		render.rectMode(CORNER);
		for(let i = 0; i < this.countBars; i++){
			let xx = (noise(time*this.velMovs[i], this.desMovs[i])-0.5)*(width-ss);
			let ww = this.ws[i]*init*end;
			let col = color(red(this.cols[i]), green(this.cols[i]), blue(this.cols[i]), 5);
			render.fill(col);
			render.rect(xx, -height*0.5, ww, height);
			render.rect(xx+ss, -height*0.5, ww, height);
		}
	}
}

class Boxes{
	constructor(){
		this.det = random(0.01)*random(1);
		this.detMask = random(0.01)*random(1);
		let size = max(width, height)*random(0.08, 0.25);
		this.cw = int(width/size);
		this.ch = int(height/size);
		this.amp = [];
		this.ms = [];
		this.ampNoiSize = [];
		this.cols = [];
		for(let j = 0; j <= this.ch; j++){
			for(let i = 0; i <= this.cw; i++){
				let ind = i+j*this.cw;
				this.amp[ind] = random(0.5, 1);
				this.ms[ind] = random(0.02);
				this.ampNoiSize[ind] = random(6)*random(1);
				this.cols[ind] = rcol();
			}
		}
	}
	update(){ 
		let init = pow(constrain(ntime*7, 0, 1), 0.6);
		let end = 1-pow(constrain(ntime*10-9, 0, 1), 0.8);

		let ani = easeInOutQuad(init*end);

		let sw = width/this.cw;	
		let sh = height/this.ch;
		let size = ((sw+sh)*0.5);	
		render.noStroke();
		for(let j = 0; j <= this.ch; j++){
			for(let i = 0; i <= this.cw; i++){
				let ind = i+j*this.cw;
				let x = i*sw-width*0.5;
				let y = j*sh-height*0.5;
				let noi = noise(x*this.det, y*this.det);
				let s = size*this.amp[ind]*constrain((j*1.0/this.ch)*2, 0.8, 1.2)*ani;
				let ms = s*0.5*sin(time*this.ms[ind]+(i+j*0.7)*0.1+pow(noi, 1.2)*8)*this.ampNoiSize[ind];
				let col = color(red(this.cols[ind]), green(this.cols[ind]), blue(this.cols[ind]), 5);
				render.fill(col);
				let noi2 = noise(x*this.detMask, y*this.detMask, time*0.1);
				if(noi2 > 0.3){
					ms *= (1-(1-noi2)*2)*3;
					render.beginShape();
					render.vertex(x-ms, y-ms);
					render.vertex(x+ms, y-ms);
					render.vertex(x+ms, y+ms);
					render.vertex(x-ms, y+ms);
					render.endShape(CLOSE);
				}
			}
		}
	}
}

class Partis{
	constructor(){
		this.count = random(random(14), 26);
		this.alp = [];
		this.col = [];
		this.size = [];
		this.velX = [];
		this.desX = [];
		this.velY = [];
		this.desY = [];
		for(let i = 0; i < this.count; i++){
			this.alp[i] = random(3)
			this.col[i] = rcol();
			this.size[i] = width*random(0.1)*random(1)*random(0.5, 1);
			this.velX[i] = random(0.014)*random(0.5, 1);
			this.desX[i] = width*random(0.2);
			this.velY[i] = random(0.014)*random(0.5, 1)*random(1);
			this.desY[i] = width*random(0.2);
		}
	}
	update(){ 
		let init = pow(constrain(ntime*7, 0, 1), 0.6);
		render.rectMode(CENTER);
		render.noStroke();
		for(let i = 0; i < this.count; i++){
			let xxx = (noise(time*this.velX[i], this.desX[i], i)*2-1)*width;
			let yyy = (noise(i, time*this.velY[i], this.desY[i])*2-1)*height;
			let sss = this.size[i]*init;
			let col = color(red(this.col[i]), green(this.col[i]), blue(this.col[i]), 5);
			render.fill(col);
			render.rect(xxx, yyy, sss, sss);
			render.fill(0, this.alp[i]*init);
			render.rect(xxx+sss*1.5, yyy+sss*1.5, sss, sss);
		}
	}
}


class Scene02{
	constructor(){
		this.scale = random(1, random(1, 1.5));
		this.moveCamera = random(1) < 0.5;
		this.velX = random(0.0081);
		this.velY = random(0.0059);
		this.desX = random(100);
		this.desY = random(100);
		this.bars = new Bars();
		this.boxes = new Boxes();
		this.partis = new Partis();
	}
	update(){
		render.push();
		if(this.moveCamera){
			let cx = (width)*(noise(time*this.velX, this.desX)*1-0.35)*0.5;
			let cy = (height)*(noise(this.desY, time*this.velY)*1-0.33)*0.5;
			render.translate(cx, cy, 0);
			render.scale(this.scale);
		}

		this.boxes.update();
		this.bars.update();
		this.partis.update();

		render.pop();
	}
}


// SCENE 03

function arc2(x, y, s1, s2, a1, a2) {
  let r1 = s1*0.5;
  let r2 = s2*0.5;
  let amp = (a2-a1);
  let ma = map(amp, 0, TWO_PI, 0, 1);
  let cc = min(max(1, int(max(r1, r2)*PI*ma*0.08)), 10);
  let da = amp/cc;
  for (let i = 0; i <= cc; i++) {
    let ang = a1+da*i;
  	render.beginShape();
    render.vertex(x+cos(ang)*r1, y+sin(ang)*r1);
    render.vertex(x+cos(ang)*r2, y+sin(ang)*r2);
    render.vertex(x+cos(ang+da)*r2, y+sin(ang+da)*r2);
    render.vertex(x+cos(ang+da)*r1, y+sin(ang+da)*r1);
  	render.endShape();
  }
}

class Arc{
	constructor(){
		this.desSize = random(0.5)*random(1)*random(0.1, 0.43);
		this.desAng = random(0.1)*random(0.1, 0.3);
		this.velAng = random(-1, 1)*random(1)*random(0.5, 1);
		this.amp = random(PI)*random(1)*random(1);
		this.str = random(80)*random(0.4, 1);
		this.col = rcol();
		this.alpha = random(40);
		this.velCol = random(3)*random(1)*random(1)*random(1);
		this.ndet = random(0.001)*random(0.2, 1);
		this.max = random(100)*random(2)*random(1);
		this.desCol = random(1000);
	}
}

class Scene03{
	constructor(){
		this.count = int(random(12, random(20, 30)));
		this.arcs = [];
		for(let i = 0; i < this.count; i++){
			this.arcs[i] = new Arc();
		}
	}
	update(){
		let init = pow(constrain(ntime*6, 0, 1), 0.8);
		let end = 1-pow(constrain(ntime*8-7, 0, 1), 0.6);
		let firstInit = constrain(time*0.1, 0, 1);

		render.noFill();
		render.noStroke();
		for(let i = 0; i < this.count; i++){
			let a = this.arcs[i];
			let dim = max(width, height)*1.5*noise(i, time*a.desSize)*sin(init*PI*0.5)*firstInit;
			let a1 = noise(time*a.desAng, i*20.1)*TAU*2+time*a.velAng;
			let a2 = a1+a.amp*init*end;
			let n = noise(time*a.ndet*2000)*a.max;
			let cc = getColor(a.desCol+time*a.velCol);
			cc = color(red(cc), green(cc), blue(cc), a.alpha*end*firstInit);
			render.fill(cc);
			if(random(1) < 0.003)
				render.fill(random(200, 255), random(40)*random(2)*firstInit);
			arc2(0, 0, dim-a.str*init-n, dim+n, a1, a2);
		}
	}
}

// manoloide 01.03/2021 
