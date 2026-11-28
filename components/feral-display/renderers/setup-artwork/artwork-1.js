p5.disableFriendlyErrors = true;

let R;
let rez = 420;

let lineLength;
let lineThickness;
let lineDepth;
let lineSpacing;

let allCOSMOS = [];

let metaLineThickness = 1.66;
let metaLineLength = 6;
let metaCHUNK = 1;
let HISPEED = 0.001;
let LOSPEED = 0.00005;
let metaSPEED = LOSPEED;

let stillLifeDim = rez;
let stillLifeDepthNeg = 0.0;
let stillLifeDepthPos = 0.0;
let depthScalar = 1.0;
let lenthScalar = 1.3;

let stillLife;
let stillLifeBlur;

let bgColor = 0;

let minCOSMOS = 1;
let maxCOSMOS = 3;
let minShapeDia = 100;
let maxShapeDia = 200;
let shapeScalar = 1.5;

let backgroundParam = "";
let numCOSMOS = -1;
let displayRed = false;
let displayGreen = false;
let displayBlue = false;
let displayWhite = false;

let viewDiagram = false;
let pauseCOSMOS = false;
let shapeSpeed = 0.0;
let rotateCounter = 0;

/* START UTILS */

let gl;
let stillLifePO2 = 2;
let dispMapTexture;
let dispBuf;

let p5renderer; 
const USE_ZERO_COPY = true; 

let LOC = {}; 

const vertCode = `precision highp float;
uniform mat4 uViewMatrix;
uniform mat4 uProjectionMatrix;
uniform mat4 uModelViewMatrix;
uniform mat4 uModelViewProjectionMatrix;
uniform vec2 canvasSize;
uniform float stillLifeDim;
uniform float stillLifePO2;
uniform float stillLifeDepthNeg;
uniform float stillLifeDepthPos;
uniform float depthScalar;
uniform float lineThickness;
uniform float lineDepth;
uniform float lineLength;
uniform float passOffsetX;  // per-pass offset replacing translate()
uniform vec3 color;
uniform sampler2D displacementMap;
attribute vec3 coordinates;
varying float vBlackMask;

float map(float value, float min1, float max1, float min2, float max2)
{
    return min2 + (value - min1) * (max2 - min2) / (max1 - min1);
}

struct Line { vec3 start; vec3 end; };

Line getLineFromDisplacementMap(int x_origin, int y_origin)
{
    Line l;

    vec2 imageCoords = vec2(x_origin, y_origin) + 0.5;
    vec2 samplePos = imageCoords / stillLifePO2;
    vec4 dispMapColor = texture2D(displacementMap, samplePos);
    float maxEdge = max(canvasSize.x, canvasSize.y);

    float x = float(x_origin);
    float y = float(y_origin);
    float z = map(dot(dispMapColor.xyz, color), 0.0, 1.0, -stillLifeDepthNeg, stillLifeDepthPos);

    l.start.x = map(x, 0.0, stillLifeDim, -maxEdge / 2.0, maxEdge / 2.0);
    l.start.y = map(y, 0.0, stillLifeDim, -maxEdge / 2.0, maxEdge / 2.0);
    l.start.z = -z;

    l.end.x = map(x, 0.0, stillLifeDim, -maxEdge / 2.0, maxEdge / 2.0) - lineDepth;
    l.end.y = map(y, 0.0, stillLifeDim, -maxEdge / 2.0, maxEdge / 2.0) - lineDepth;
    l.end.z = -lineLength - z;

    return l;
}

void main()
{
    bool lineStart = (coordinates.y == 0.0) || (coordinates.y == 1.0) || (coordinates.y == 3.0);

    int imageCoordsY = int(floor(coordinates.x / stillLifeDim));
    int imageCoordsX = int(floor(coordinates.x - float(imageCoordsY) * stillLifeDim));

    vec2 imageCoords2 = vec2(float(imageCoordsX) + 0.5, float(imageCoordsY) + 0.5);
    vec2 samplePos2 = imageCoords2 / stillLifePO2;
    vec3 pix = texture2D(displacementMap, samplePos2).rgb;
    float sumRGB = pix.r + pix.g + pix.b;
    vBlackMask = step(0.05, sumRGB);  // 0 if near-black, 1 otherwise

    Line l = getLineFromDisplacementMap(imageCoordsX, imageCoordsY);

    vec3 vertexPosition = lineStart ? l.start : l.end;
    vertexPosition.x += passOffsetX; // replaces per-pass translate()

    vec4 finalPos = uModelViewProjectionMatrix * vec4(vertexPosition, 1.0);

    vec4 projLineStart = uModelViewProjectionMatrix * vec4(l.start, 1.0);
    vec4 projLineEnd = uModelViewProjectionMatrix * vec4(l.end, 1.0);
    vec2 lineDirection = projLineStart.xy - projLineEnd.xy;
    lineDirection = vec2(-lineDirection.y, lineDirection.x);
    float maxEdge = max(canvasSize.x, canvasSize.y);
    lineDirection /= canvasSize / maxEdge;
    vec2 displacement = normalize(lineDirection);
    displacement /= canvasSize;
    displacement *= lineThickness;

    if (coordinates.y == 0.0) finalPos.xy -= displacement;
    if (coordinates.y == 2.0) finalPos.xy -= displacement;
    if (coordinates.y == 5.0) finalPos.xy -= displacement;
    if (coordinates.y == 1.0) finalPos.xy += displacement;
    if (coordinates.y == 3.0) finalPos.xy += displacement;
    if (coordinates.y == 4.0) finalPos.xy += displacement;

    finalPos.z = 0.0;
    gl_Position = finalPos;
}
`;

const fragCode = `precision highp float;
uniform vec3 color;
uniform vec3 displayColor;
varying float vBlackMask;

void main() {
    if (vBlackMask < 0.5 && (color.r > 0.5 || color.g > 0.5 || color.b > 0.5)) discard;
    gl_FragColor = vec4(displayColor, 1.0);
}
`;

let triangle_buffer;
let numTris;

function setupGeometry() {
  let triangles = [];
  numTris = 0;
  for (let y = 0; y < rez; y += 2) {
    for (let x = 0; x < rez; x += 2) {
      triangles.push(x + y * stillLifeDim, 0, 0);
      triangles.push(x + y * stillLifeDim, 1, 0);
      triangles.push(x + y * stillLifeDim, 2, 0);
      triangles.push(x + y * stillLifeDim, 3, 0);
      triangles.push(x + y * stillLifeDim, 4, 0);
      triangles.push(x + y * stillLifeDim, 5, 0);
      numTris += 6;
    }
  }
  triangle_buffer = gl.createBuffer();
  gl.bindBuffer(gl.ARRAY_BUFFER, triangle_buffer);
  gl.bufferData(gl.ARRAY_BUFFER, new Float32Array(triangles), gl.STATIC_DRAW);
  gl.bindBuffer(gl.ARRAY_BUFFER, null);
}

let shaderProgram;

function setupShaders() {
  let vertShader = gl.createShader(gl.VERTEX_SHADER);
  gl.shaderSource(vertShader, vertCode);
  gl.compileShader(vertShader);
  const vertError = gl.getShaderInfoLog(vertShader);
  if (vertError.length > 0) throw vertError;

  let fragShader = gl.createShader(gl.FRAGMENT_SHADER);
  gl.shaderSource(fragShader, fragCode);
  gl.compileShader(fragShader);
  const fragError = gl.getShaderInfoLog(fragShader);
  if (fragError.length > 0) throw fragError;

  shaderProgram = gl.createProgram();
  gl.attachShader(shaderProgram, vertShader);
  gl.attachShader(shaderProgram, fragShader);
  gl.linkProgram(shaderProgram);

  LOC = {
    coordinates: gl.getAttribLocation(shaderProgram, "coordinates"),
    uViewMatrix: gl.getUniformLocation(shaderProgram, "uViewMatrix"),
    uProjectionMatrix: gl.getUniformLocation(
      shaderProgram,
      "uProjectionMatrix"
    ),
    uModelViewMatrix: gl.getUniformLocation(shaderProgram, "uModelViewMatrix"),
    uMVP: gl.getUniformLocation(shaderProgram, "uModelViewProjectionMatrix"),
    canvasSize: gl.getUniformLocation(shaderProgram, "canvasSize"),
    stillLifeDim: gl.getUniformLocation(shaderProgram, "stillLifeDim"),
    stillLifePO2: gl.getUniformLocation(shaderProgram, "stillLifePO2"),
    depthScalar: gl.getUniformLocation(shaderProgram, "depthScalar"),
    stillLifeDepthNeg: gl.getUniformLocation(
      shaderProgram,
      "stillLifeDepthNeg"
    ),
    stillLifeDepthPos: gl.getUniformLocation(
      shaderProgram,
      "stillLifeDepthPos"
    ),
    lineThickness: gl.getUniformLocation(shaderProgram, "lineThickness"),
    lineLength: gl.getUniformLocation(shaderProgram, "lineLength"),
    lineDepth: gl.getUniformLocation(shaderProgram, "lineDepth"),
    passOffsetX: gl.getUniformLocation(shaderProgram, "passOffsetX"),
    color: gl.getUniformLocation(shaderProgram, "color"),
    displayColor: gl.getUniformLocation(shaderProgram, "displayColor"),
    displacementMap: gl.getUniformLocation(shaderProgram, "displacementMap"),
  };
}

function createDisplacementMapTexture() {
  dispMapTexture = gl.createTexture();
  while (stillLifePO2 < stillLifeDim) stillLifePO2 *= 2;

  dispBuf = new Uint8Array(stillLifePO2 * stillLifePO2 * 4);

  gl.bindTexture(gl.TEXTURE_2D, dispMapTexture);
  gl.texImage2D(
    gl.TEXTURE_2D,
    0,
    gl.RGBA,
    stillLifePO2,
    stillLifePO2,
    0,
    gl.RGBA,
    gl.UNSIGNED_BYTE,
    null
  );
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.NEAREST);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.NEAREST);
  gl.pixelStorei(gl.UNPACK_ALIGNMENT, 1);
  gl.bindTexture(gl.TEXTURE_2D, null);
}

function transferDisplacementMap() {
  if (
    USE_ZERO_COPY &&
    p5renderer &&
    typeof p5renderer.getTexture === "function" &&
    stillLifeBlur
  ) {
    if (stillLife && stillLife._renderer && stillLife._renderer._gl)
      stillLife._renderer._gl.flush();
    if (stillLifeBlur && stillLifeBlur._renderer && stillLifeBlur._renderer._gl)
      stillLifeBlur._renderer._gl.flush();

    const texObj = p5renderer.getTexture(stillLifeBlur);
    if (texObj) {
      if (typeof texObj.update === "function") texObj.update();

      const glTex = texObj.glTex || texObj.texture; 
      if (glTex) {
        gl.activeTexture(gl.TEXTURE2);
        gl.bindTexture(gl.TEXTURE_2D, glTex);
        gl.uniform1i(LOC.displacementMap, 2);
        stillLifePO2 = stillLifeDim;
        return; 
      }
    }
  }

  if (
    stillLifePO2 < stillLifeDim ||
    (stillLifePO2 & (stillLifePO2 - 1)) !== 0
  ) {
    stillLifePO2 = 2;
    while (stillLifePO2 < stillLifeDim) stillLifePO2 *= 2;
  }

  const img = stillLifeBlur.get(); 
  img.loadPixels();

  const W = stillLifeDim,
    PO2 = stillLifePO2;
  const src = img.pixels; 

  for (let y = 0; y < W; ++y) {
    const rowOffPO2 = y * PO2 * 4;
    const rowOffW = y * W * 4;
    for (let x = 0; x < W; ++x) {
      const s = rowOffW + (x << 2);
      const d = rowOffPO2 + (x << 2);
      dispBuf[d + 0] = src[s + 0];
      dispBuf[d + 1] = src[s + 1];
      dispBuf[d + 2] = src[s + 2];
      dispBuf[d + 3] = 255;
    }
  }

  gl.bindTexture(gl.TEXTURE_2D, dispMapTexture);
  gl.texSubImage2D(
    gl.TEXTURE_2D,
    0,
    0,
    0,
    PO2,
    PO2,
    gl.RGBA,
    gl.UNSIGNED_BYTE,
    dispBuf
  );
  gl.bindTexture(gl.TEXTURE_2D, null);

  gl.activeTexture(gl.TEXTURE2);
  gl.bindTexture(gl.TEXTURE_2D, dispMapTexture);
  gl.uniform1i(LOC.displacementMap, 2);
}

function setMatrices() {
  const viewMatrix = this._renderer._curCamera.cameraMatrix;
  const projectionMatrix = this._renderer.uPMatrix;
  const modelViewMatrix = this._renderer.uMVMatrix;
  const modelViewProjectionMatrix = modelViewMatrix.copy();
  modelViewProjectionMatrix.mult(projectionMatrix);

  gl.uniformMatrix4fv(LOC.uViewMatrix, false, viewMatrix.mat4);
  gl.uniformMatrix4fv(LOC.uProjectionMatrix, false, projectionMatrix.mat4);
  gl.uniformMatrix4fv(LOC.uModelViewMatrix, false, modelViewMatrix.mat4);
  gl.uniformMatrix4fv(LOC.uMVP, false, modelViewProjectionMatrix.mat4);
}

function setUniforms() {
  gl.uniform2f(LOC.canvasSize, width, height);
  gl.uniform1f(LOC.stillLifeDim, stillLifeDim);
  gl.uniform1f(LOC.stillLifePO2, stillLifePO2);
  gl.uniform1f(LOC.depthScalar, depthScalar);
  gl.uniform1f(LOC.stillLifeDepthNeg, stillLifeDepthNeg * depthScalar);
  gl.uniform1f(LOC.stillLifeDepthPos, stillLifeDepthPos * depthScalar);
  gl.uniform1f(LOC.lineThickness, lineThickness);
  gl.uniform1f(LOC.lineLength, lineLength);
  gl.uniform1f(LOC.lineDepth, lineDepth);
}

function beginLinesShader() {
  gl.useProgram(shaderProgram);
  gl.bindBuffer(gl.ARRAY_BUFFER, triangle_buffer);

  transferDisplacementMap();
  setUniforms();

  gl.vertexAttribPointer(LOC.coordinates, 3, gl.FLOAT, false, 0, 0);
  gl.enableVertexAttribArray(LOC.coordinates);

  gl.enable(gl.BLEND);
  gl.blendEquation(gl.FUNC_ADD);
  gl.blendFunc(gl.ONE, gl.ONE);

  gl.disable(gl.DEPTH_TEST);
  gl.clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT);
  gl.viewport(0, 0, canvas.width, canvas.height);
}

function endLinesShader() {
  gl.useProgram(null);
}

/* END UTILS */

function renderStillLife() {
  beginLinesShader();

  setMatrices();

  gl.uniform1f(LOC.passOffsetX, 0.0);
  gl.uniform3f(LOC.color, 1, 0, 0);
  if (displayWhite) {
    gl.uniform3f(LOC.displayColor, 0.8, 0.8, 0.8);
  } else {
    gl.uniform3f(LOC.displayColor, 1, 0, 0);
  }
  if (displayRed || displayWhite) gl.drawArrays(gl.TRIANGLES, 0, numTris);

  gl.uniform1f(LOC.passOffsetX, lineSpacing);
  gl.uniform3f(LOC.color, 0, 1, 0);
  if (displayWhite) {
    gl.uniform3f(LOC.displayColor, 0.8, 0.8, 0.8);
  } else {
    gl.uniform3f(LOC.displayColor, 0, 1, 0);
  }
  if (displayGreen || displayWhite) gl.drawArrays(gl.TRIANGLES, 0, numTris);

  gl.uniform1f(LOC.passOffsetX, 2.0 * lineSpacing);
  gl.uniform3f(LOC.color, 0, 0, 1);
  if (displayWhite) {
    gl.uniform3f(LOC.displayColor, 0.8, 0.8, 0.8);
  } else {
    gl.uniform3f(LOC.displayColor, 0, 0, 1);
  }
  if (displayBlue || displayWhite) gl.drawArrays(gl.TRIANGLES, 0, numTris);

  endLinesShader();
}

function setupTransform() {
  scale(1.2);
  ortho();
  rotateY(0.33);
  rotateX(-0.33);
}

class COSMOS {
  constructor(num) {
    if (num === 0) {
      if (width > height) {
        this.x = map(R.random_num(0, 1), 0, 1, -stillLifeBlur.width/8, stillLifeBlur.width/8);
        this.y = 0;
      } else {
        this.x = 0;
        this.y = map(R.random_num(0, 1), 0, 1, -stillLifeBlur.height/8, stillLifeBlur.height/8);
      }
    } else if (num === 1) {
      if (width > height) {
        if (allCOSMOS[0].x > 0) {
          this.x = map(R.random_num(0, 1), 0, 1, -stillLifeBlur.width/4, -stillLifeBlur.width/2);
        } else {
          this.x = map(R.random_num(0, 1), 0, 1, stillLifeBlur.width/4, stillLifeBlur.width/2);
        }
        this.y = 0;
      } else {
        this.x = 0;
        if (allCOSMOS[0].x > 0) {
          this.y = map(R.random_num(0, 1), 0, 1, -stillLifeBlur.height/2, 0);
        } else {
          this.y = map(R.random_num(0, 1), 0, 1, 0, stillLifeBlur.height/2);
        }
      }
    } else {
      if (width > height) {
        this.x = map(R.random_num(0, 1), 0, 1, -stillLifeBlur.width/2, stillLifeBlur.width/2);
        this.y = 0;
      } else {
        this.x = 0;
        this.y = map(R.random_num(0, 1), 0, 1, -stillLifeBlur.height/2, stillLifeBlur.height/2);
      }
    }

    this.rx = R.random_num(0, 1) * TWO_PI;
    this.ry = R.random_num(0, 1) * TWO_PI;
    this.dia = map(R.random_num(0, 1), 0, 1, minShapeDia, maxShapeDia) * shapeScalar;
  }
}

function setup() {
  let c = createCanvas(
    window.innerWidth,
    window.innerHeight,
    WEBGL,
    document.getElementById("cosmos")
  );
  
  noCursor();

  R = new Random();
  gl = c.GL;
  p5renderer = this._renderer;

  setupGeometry();
  setupShaders();
  createDisplacementMapTexture();

  stillLife = createGraphics(stillLifeDim, stillLifeDim, WEBGL);
  stillLifeBlur = createGraphics(stillLifeDim, stillLifeDim, WEBGL);
  stillLifeBlur.imageMode(CENTER);
  
  if (width/height > 1.5 || width/height < 0.75) {
    shapeScalar = 1.0;
  }

  generateStillLife();
  
  stillLifeDepthNeg = 30;
  stillLifeDepthPos = 30;

  setVertexColors();
}

function generateStillLife() {

  stillLifeBlur.background(0);

  backgroundColor = 0;

  numCOSMOS = int(R.random_num(minCOSMOS, maxCOSMOS + 1));

  let chunks = [1, 1, 1, 4, 4, 7, 7];
  let chunksSlim = [1, 1, 1, 4, 4];
  let chunksChunk = [1, 1, 7, 7, 10, 10];
  metaCHUNK = chunks[floor(R.random_num(0, chunks.length))];

  displayRed = false;
  displayGreen = false;
  displayBlue = false;
  displayWhite = false;
  
  let tr = R.random_num(0, 1);
  
  if (tr < 0.1) {
    let rrrr = R.random_num(0, 1);
    if (rrrr > 0.66) {
      displayRed = true;
    } else if (rrrr > 0.33) {
      displayGreen = true;
    } else {
      displayBlue = true;
    }
    metaCHUNK = chunksChunk[floor(R.random_num(0, chunksChunk.length))];
  } else if (tr < 0.7) {
    displayRed = true;
    displayGreen = true;
    displayBlue = true;
    metaCHUNK = chunksSlim[floor(R.random_num(0, chunksSlim.length))];
  } else {
    displayWhite = true;
  }
  
  for (let i = 0; i < numCOSMOS; i++) {
    allCOSMOS[i] = new COSMOS(i);
  }
}

function draw() {
  
  let scaleFactor = max(width, height) / 1500.0;

  lineThickness = metaLineThickness * scaleFactor;
  lineDepth = 2 * scaleFactor;
  lineLength = metaLineLength * lenthScalar * scaleFactor;
  lineSpacing = 2.0 * scaleFactor;
  depthScalar = map(width, 600, 3840, 0.8, 3.2);

  if (!pauseCOSMOS) {
    shapeSpeed = rotateCounter;
    rotateCounter += metaSPEED;
  }

  stillLife.background(0);

  for (let i = 0; i < numCOSMOS; i++) {
    let x = allCOSMOS[i].x;
    let y = allCOSMOS[i].y;
    let rx = allCOSMOS[i].rx;
    let ry = allCOSMOS[i].ry;
    let rd = allCOSMOS[i].dia;
    doce(x, y, -30, rx, ry, 0, rd, stillLife);
  }

  stillLifeBlur.tint(255, 51);
  stillLifeBlur.image(stillLife, 0, 0);

  background(bgColor);

  push();
  setupTransform();
  renderStillLife();
  pop();
  
}

function keyPressed() {
  if (key == " ") {
    generateStillLife();
  } else if (key == "p" || key == "P") {
    pauseCOSMOS = !pauseCOSMOS;
  } else if (key == "s" || key == "S") {
    metaSPEED += 0.0002;
    if (metaSPEED > HISPEED) {
      metaSPEED = LOSPEED;
    }
  } else if (key == "r" || key == "r") {
    displayRed = !displayRed;
  } else if (key == "g" || key == "G") {
    displayGreen = !displayGreen;
  } else if (key == "b" || key == "b") {
    displayBlue = !displayBlue;
  } else if (key == "w" || key == "W") {
    displayWhite = !displayWhite;
  }
  noCursor();
}

function windowResized() {
  resizeCanvas(window.innerWidth, window.innerHeight);
  noCursor();
}

let phi = 1.61803398875;
let b = 1 / phi;
let c = 2 - phi;

let faces = [
  [
    [c, 0, 1],
    [-c, 0, 1],
    [-b, b, b],
    [0, 1, c],
    [b, b, b],
  ],
  [
    [-c, 0, 1],
    [c, 0, 1],
    [b, -b, b],
    [0, -1, c],
    [-b, -b, b],
  ],
  [
    [c, 0, -1],
    [-c, 0, -1],
    [-b, -b, -b],
    [0, -1, -c],
    [b, -b, -b],
  ],
  [
    [-c, 0, -1],
    [c, 0, -1],
    [b, b, -b],
    [0, 1, -c],
    [-b, b, -b],
  ],
  [
    [0, 1, -c],
    [0, 1, c],
    [b, b, b],
    [1, c, 0],
    [b, b, -b],
  ],
  [
    [0, 1, c],
    [0, 1, -c],
    [-b, b, -b],
    [-1, c, 0],
    [-b, b, b],
  ],
  [
    [0, -1, -c],
    [0, -1, c],
    [-b, -b, b],
    [-1, -c, 0],
    [-b, -b, -b],
  ],
  [
    [0, -1, c],
    [0, -1, -c],
    [b, -b, -b],
    [1, -c, 0],
    [b, -b, b],
  ],
  [
    [1, c, 0],
    [1, -c, 0],
    [b, -b, b],
    [c, 0, 1],
    [b, b, b],
  ],
  [
    [1, -c, 0],
    [1, c, 0],
    [b, b, -b],
    [c, 0, -1],
    [b, -b, -b],
  ],
  [
    [-1, c, 0],
    [-1, -c, 0],
    [-b, -b, -b],
    [-c, 0, -1],
    [-b, b, -b],
  ],
  [
    [-1, -c, 0],
    [-1, c, 0],
    [-b, b, b],
    [-c, 0, 1],
    [-b, -b, b],
  ],
];

let faceColors = [];
function setVertexColors() {
  faceColors = new Array(faces.length);
  for (let f = 0; f < faces.length; f++) {
    faceColors[f] = new Array(faces[0].length);
    for (let j = 0; j < faces[0].length; j++) {
      faceColors[f][j] = [
        R.random_num(0, 255),
        R.random_num(0, 255),
        R.random_num(0, 255),
      ];
    }
  }
}

function doce(x, y, z, rx, ry, rz, sc, p) {
  p.noStroke();
  p.push();
  p.translate(x, y, z);

  p.rotateX(rx - shapeSpeed);
  p.rotateY(ry + shapeSpeed);

  p.scale(sc);
  p.strokeWeight(metaCHUNK/1.2);
  p.noFill();
  p.blendMode(ADD);

  for (let f = 0; f < faces.length; f++) {
    p.beginShape();
    for (let j = 0; j < faces[0].length; j++) {
      const col = faceColors[f][j];
      p.stroke(col[0], col[1], col[2]);
      const v = faces[f][j];
      p.vertex(v[0], v[1], v[2]);
    }
    p.endShape(CLOSE);
  }
  p.pop();
}

class Random {
  constructor() {
    this.useA = false;
    let sfc32 = function (uint128Hex) {
      let a = parseInt(uint128Hex.substring(0, 8), 16);
      let b = parseInt(uint128Hex.substring(8, 16), 16);
      let c = parseInt(uint128Hex.substring(16, 24), 16);
      let d = parseInt(uint128Hex.substring(24, 32), 16);
      return function () {
        a |= 0;
        b |= 0;
        c |= 0;
        d |= 0;
        let t = (((a + b) | 0) + d) | 0;
        d = (d + 1) | 0;
        a = b ^ (b >>> 9);
        b = (c + (c << 3)) | 0;
        c = (c << 21) | (c >>> 11);
        c = (c + t) | 0;
        return (t >>> 0) / 4294967296;
      };
    };
    this.prngA = new sfc32(tokenData.hash.substring(2, 34));
    this.prngB = new sfc32(tokenData.hash.substring(34, 66));
    for (let i = 0; i < 1e6; i += 2) {
      this.prngA();
      this.prngB();
    }
  }
  random_dec() {
    this.useA = !this.useA;
    return this.useA ? this.prngA() : this.prngB();
  }
  random_num(a, b) {
    return a + (b - a) * this.random_dec();
  }
  random_int(a, b) {
    return Math.floor(this.random_num(a, b + 1));
  }
  random_bool(p) {
    return this.random_dec() < p;
  }
  random_choice(list) {
    return list[this.random_int(0, list.length - 1)];
  }
}