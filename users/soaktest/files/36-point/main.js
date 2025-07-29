// 36 Points (2019-2022)
// by Sage Jenson (sagejenson.com / @mxsage);

// License Creative Commons Attribution-NonCommercial-ShareAlike 3.0 Unported License
// Contact the author for other licensing options (sagejenson.com / @mxsage)

const edition = 1; // [1 - 36]

const paramsString =
`
0.000, 4.000, 0.300, 0.100, 51.32, 20.00, 0.410, 4.000, 0.000, 0.100, 6.000, 0.100, 0.000, 0.000, 0.400, 0.705, 1.000, 0.300, 0.250, 8.0,   // pure_multiscale
0.000, 28.04, 14.53, 0.090, 1.000, 0.000, 0.010, 1.400, 1.120, 0.830, 0.000, 0.000, 0.570, 0.030, 0.070, 0.986, 1.000, 0.230, 0.166, 6.0,   // hex_hole_open
17.92, 0.000, 0.000, 0.520, 0.000, 0.000, 0.180, 0.000, 0.000, 0.100, 6.050, 0.170, 0.000, 0.000, 0.040, 0.973, 1.000, 0.530, 0.455, 16.,   // vertebrata
3.000, 0.000, 0.000, 0.350, 0.000, 0.000, 0.000, 0.570, 2.000, 0.010, 4.000, 0.020, 0.300, 0.000, 0.110, 0.945, 1.000, 0.180, 0.248, 16.,   // traffic_many_lanes
13.95, 7.460, 0.110, 4.040, 5.000, 0.520, 0.490, 0.580, 0.180, 7.590, 3.040, 0.160, 4.760, 0.000, 0.610, 0.975, 1.000, 0.348, 0.172, 5.0,   // tactile_extreme
3.000, 10.17, 0.400, 1.030, 2.300, 2.000, 1.420, 20.00, 0.750, 0.830, 1.560, 0.110, 1.070, 0.000, 0.200, 0.951, 10.00, 0.150, 0.248, 16.,   // star_network
0.000, 8.510, 0.190, 0.610, 0.000, 0.000, 3.350, 0.000, 0.000, 0.750, 12.62, 0.060, 0.000, 0.000, 0.270, 0.904, 1.000, 0.060, 0.042, 7.0,   // enmeshed_singularities
0.000, 0.820, 0.030, 0.1800, 0.00, 0.000, 0.260, 0.000, 0.000, 0.000, 20.00, 0.650, 0.200, 0.900, 0.140, 0.939, 1.000, 0.470, 0.430, 10.,   // waves_upturn
0.000, 8.440, 0.080, 4.820, 0.000, 0.000, 1.190, 0.000, 0.000, 0.000, 0.330, 0.010, 0.000, 0.000, 0.040, 0.980, 1.000, 0.320, 0.172, 7.0,   // turing
1.660, 19.26, 0.060, 1.260, 0.000, 0.000, 1.650, 0.000, 0.000, 0.060, 5.740, 0.080, 0.000, 3.040, 0.110, 0.988, 3.000, 0.134, 0.221, 19.,   // petri_worms
0.000, 17.54, 0.080, 0.640, 0.000, 0.000, 1.800, 0.000, 0.000, 0.100, 20.00, 0.060, 0.400, 0.000, 0.200, 0.939, 1.000, 0.200, 0.283, 14.,   // a_rooting
1.500, 1.940, 0.280, 1.730, 1.120, 0.710, 0.180, 2.220, 0.850, 0.500, 4.130, 0.110, 1.120, 0.000, 0.020, 0.850, 1.000, 0.140, 0.234, 11.,   // more_individuals
8.340, 3.860, 0.030, 1.210, 1.400, 0.300, 1.130, 5.500, 0.390, 17.85, 8.510, 0.960, 0.000, 7.140, 0.020, 0.781, 1.000, 0.200, 0.166, 16.,   // slow_metastructure
2.870, 3.040, 0.280, 0.090, 0.000, 0.000, 0.440, 0.850, 0.000, 0.000, 2.220, 0.140, 0.300, 0.850, 0.020, 0.891, 1.000, 0.140, 0.166, 21.,   // sloppy_bucky
0.140, 1.120, 0.190, 0.270, 1.400, 0.000, 1.130, 2.000, 0.390, 0.750, 2.220, 0.190, 0.000, 7.140, 0.210, 0.795, 1.000, 0.120, 0.166, 19.,   // massive_structure
0.001, 2.540, 0.080, 0.000, 0.000, 0.000, 3.350, 0.000, 0.000, 0.100, 12.62, 0.060, 0.000, 0.000, 0.270, 0.877, 1.000, 0.250, 0.344, 5.0,   // speed_modulation
0.000, 20.00, 0.080, 5.280, 0.000, 0.000, 5.200, 0.000, 0.000, 1.440, 1.560, 0.060, 1.810, 0.000, 0.050, 0.987, 1.000, 0.280, 0.172, 16.,   // emergent_hex_waves
0.000, 17.26, 0.280, 0.350, 1.120, 0.660, 1.470, 0.570, 1.020, 0.750, 19.18, 0.390, 0.000, 1.940, 0.130, 0.959, 1.000, 0.110, 0.135, 21.,   // formalisms
0.000, 89.60, 20.00, 1.300, 0.000, 0.000, 1.300, 1.400, 1.070, 0.750, 69.08, 2.220, 0.300, 0.000, 0.080, 0.959, 1.000, 0.160, 0.332, 10.,   // growing_on_a_sea_of_sand
4.240, 75.92, 0.000, 4.390, 0.000, 0.000, 1.300, 171.7, 20.00, 6.220, 7.520, 1.120, 0.000, 0.000, 0.060, 0.877, 5.000, 0.230, 0.166, 11.,   // grid_of_sorts
17.92, 89.60, 3.040, 2.670, 34.88, 10.70, 0.350, 294.8, 0.000, 0.001, 82.76, 20.00, 0.000, 0.000, 0.005, 0.999, 1.000, 0.330, 0.289, 6.0,   // negotiation_of_highways
0.000, 28.04, 20.00, 0.180, 26.74, 20.00, 0.010, 1.400, 1.120, 0.830, 0.000, 0.000, 2.540, 0.000, 0.120, 0.959, 1.000, 0.230, 0.166, 5.0,   // transmission_tower
2.000, 28.04, 0.000, 0.090, 1.000, 0.000, 0.800, 2.080, 0.000, 0.000, 2.000, 0.030, 0.820, 0.000, 0.050, 0.889, 1.000, 0.200, 0.394, 16.,   // sacred_network_nodules
0.000, 0.850, 0.010, 0.350, 1.400, 0.000, 1.810, 0.570, 1.450, 0.010, 4.000, 0.020, 0.300, 0.000, 0.110, 0.945, 1.000, 0.070, 0.049, 16.,   // positive_negative_space
1.660, 20.00, 33.19, 1.030, 39.03, 2.540, 2.650, 364.8, 8.200, 0.050, 2.150, 2.540, 0.000, 0.000, 0.001, 0.975, 1.000, 0.160, 0.115, 14.,   // circular_consolidation
0.000, 9.000, 2000., 1.030, 39.03, 2.540, 2.650, 174.3, 8.200, 6.360, 5.000, 20.00, 0.000, 0.000, 0.001, 0.975, 1.000, 0.080, 0.115, 14.,   // radiative_nexus
17.92, 89.60, 3.040, 2.670, 34.88, 10.70, 3.350, 294.8, 0.000, 0.001, 69.76, 116.4, 0.000, 0.000, 0.005, 0.999, 1.000, 0.330, 0.289, 10.,   // unfold_time_but_only_in_a_line
0.000, 20.00, 3.000, 0.260, 2.150, 4.760, 0.410, 6.600, 12.62, 0.300, 6.600, 0.037, 0.400, 0.040, 0.030, 0.926, 1.000, 0.450, 0.459, 10.,   // ink_on_white
0.000, 89.60, 20.00, 1.300, 0.000, 0.000, 0.180, 1.400, 1.070, 0.750, 69.08, 2.220, 0.300, 0.000, 0.080, 0.960, 1.000, 0.160, 0.332, 7.0,   // network_time
0.000, 0.800, 0.020, 0.100, 1.000, 0.000, 0.260, 0.100, 2.790, 0.830, 32.88, 37.74, 0.090, 0.330, 0.100, 0.939, 1.000, 0.430, 0.262, 3.0,   // inverse_network
27.50, 2.000, 2.540, 0.880, 26.74, 0.000, 0.090, 267.4, 1.400, 0.100, 5.000, 7.410, 1.400, 14.25, 0.140, 0.754, 1.000, 0.600, 0.627, 11.,   // vanishing_points
5.350, 6.000, 0.000, 0.100, 1.000, 0.000, 0.180, 1.000, 0.000, 0.000, 2.150, 0.330, 0.000, 0.000, 0.100, 0.840, 2.000, 0.230, 0.164, 16.,   // neuron_cluster
0.000, 6.000, 100.0, 0.157, 1.000, 1.070, 0.000, 1.000, 5.000, 0.830, 5.000, 20.00, 0.400, 0.000, 0.003, 0.914, 1.000, 0.250, 0.361, 6.0,   // scaling_nodule_emergence
0.005, 6.000, 205.3, 0.000, 1.000, 1.000, 0.180, 2.200, 20.00, 0.830, 3.000, 1.320, 0.400, 0.000, 0.001, 0.939, 1.000, 0.150, 0.361, 6.0,   // probe_emergence_from_line
0.000, 15.00, 8.600, 0.030, 1.000, 0.000, 0.340, 2.000, 1.070, 0.220, 15.00, 0.100, 2.300, 0.820, 1.000, 0.705, 1.000, 0.420, 0.373, 8.0,   // hyp_offset
0.000, 0.000, 0.000, 0.000, 0.000, 0.000, 0.000, 0.000, 0.000, 1.000, 0.000, 0.000, 0.000, 0.000, 0.000, 0.000, 0.000, 0.230, 0.166, 4.0,   // noise
0.000, 32.88, 402.0, 0.410, 3.000, 0.000, 0.100, 0.000, 0.000, 0.300, 6.000, 0.000, 0.000, 0.000, 0.090, 0.914, 1.000, 0.460, 0.290, 6.0,   // strike
5.350, 2.150, 0.000, 0.340, 20.59, 0.000, 0.490, 0.100, 2.790, 0.830, 125.1, 45.11, 0.090, 0.000, 0.190, 0.975, 1.000, 0.550, 0.213, 6.0,   // suture
0.000, 100.5, 20.00, 0.180, 14.44, 0.000, 1.260, 0.000, 0.000, 0.830, 75.91, 0.860, 0.300, 0.000, 0.390, 0.975, 2.000, 0.250, 0.250, 11.,   // surface_tension_sharp
0.000, 0.800, 0.020, 0.340, 20.59, 0.000, 0.260, 0.100, 2.790, 0.830, 125.1, 45.11, 0.580, 0.330, 0.190, 0.975, 1.000, 0.520, 0.238, 5.0,   // pincushion
0.000, 0.800, 0.020, 5.200, 1.000, 0.000, 0.260, 0.100, 2.790, 0.830, 32.88, 37.74, 0.090, 0.330, 0.100, 0.939, 1.000, 0.450, 0.189, 6.0,   // clear_spaghetti
17.92, 89.60, 3.040, 2.670, 34.88, 10.70, 5.770, 294.8, 0.000, 0.001, 82.76, 20.00, 0.000, 0.000, 0.005, 0.999, 1.000, 0.330, 0.289, 10.,   // negotiation_of_zoning
1.829, 23.65, 0.029, 0.674, 0.500, 0.000, 1.224, 1.039, 0.000, 0.029, 3.869, 0.054, 0.409, 1.519, 0.080, 0.938, 2.000, 0.065, 0.307, 18.,   // hexa1833
`;

let parameterIndex = edition - 1;
const maxParams = 43;
const parameterLength = 20;
let interpolant = false;

const tempBlurArray = paramsString.split("\n");
tempBlurArray.shift();
for (let i = 0; i < tempBlurArray.length; ++i)
{
    tempBlurArray[i] = tempBlurArray[i].split(",");
}

let blurArray = [];
let paramsArray = [];
let paramNames = [];
let frame = 0;
for (let i = 0; i < tempBlurArray.length; ++i)
{
    if (tempBlurArray[i].length > 16)
    {
        blurArray.push(parseInt(tempBlurArray[i][16]));
    }
    if (tempBlurArray[i].length >= 19)
    {
        paramsArray.push(new Float32Array(tempBlurArray[i].slice(0, 20)));
    }
    let name = tempBlurArray[i][20] + "";
    name = name.replace(/\s|\//g, "");
    paramNames.push(name);
}

let useHalf = false;
const isFirefox = navigator.userAgent.toLowerCase().includes('firefox');
if (isFirefox)
{
    console.log("Firefox detected... I suggest using Chrome unfortunately.");
    useHalf = false;
}

let iOS = ['iPad', 'iPhone', 'iPod'].includes(navigator.platform) || (navigator.userAgent.includes("Mac") && "ontouchend" in document);

if (iOS)
{
    useHalf = true;
}

if (useHalf)
{
    paramsArray[16][15] = 0.988;
    paramsArray[18][15] = 0.977;
    paramsArray[25][15] = 0.990;
    paramsArray[27][15] = 0.965;
    paramsArray[28][15] += 0.02;
    paramsArray[29][15] -= 0.002;
    paramsArray[32][15] += 0.04;
    paramsArray[33][15] += 0.045;
    paramsArray[35][15] += 0.1;
}

let mobile = false;
if (iOS) mobile = true;

console.log("36 Points (2019-2022)\nPoint " + edition.toString().padStart(2, '0') + " (" + paramNames[edition-1] + ")" + "\nby Sage Jenson\n(sagejenson.com / @mxsage)");

const particleRenderVert =
`#version 300 es\n
precision highp float;

in vec2 i_P;
uniform float pointsize;
uniform float dotSize;

void main() {
  gl_PointSize = pointsize;
  gl_Position = vec4(i_P, 0.0, 1.0);

  if (gl_VertexID == 0)
  {
    gl_PointSize = dotSize;
  }
}`;

const particleRenderFrag =
`#version 300 es\n
precision highp float;

out vec4 FragColor;

uniform float[19] v;

uniform int deposit;

void main() {
    float opacity;
    if (deposit == 1)
    {
        opacity = v[14];
    }
    else
    {
        opacity = v[17];
    }

    if (dot(gl_PointCoord - 0.5, gl_PointCoord - 0.5) > 0.25) discard;
    else FragColor = vec4(1., 1., 1.,opacity);
}`;

const passthroughFrag =
`#version 300 es\n
precision highp float;
in float v_A;
void main() { discard; }`;

const particleUpdateVert =
`#version 300 es\n

// License Creative Commons Attribution-NonCommercial-ShareAlike 3.0 Unported License\n
// Full license: https://creativecommons.org/licenses/by-nc-sa/3.0/legalcode\n
// Contact the author for other licensing options (sagejenson.com / @mxsage)\n

precision highp float;

uniform sampler2D u_trail;

in vec2 i_P;
in float i_A;
in float i_T;

out vec2 v_P;
out float v_A;
out float v_T;

uniform vec2 i_dim;
uniform int pen;

uniform float[19] v;
uniform float[8] mps;

uniform int frame;

vec2 bd(vec2 pos)
{
    pos *= .5;
    pos += vec2(.5);
    pos -= floor(pos);
    pos -= vec2(.5);
    pos *= 2.;
    return pos;
}

float gn(in vec2 coordinate, in float seed){
    return fract(tan(distance(coordinate*(seed+0.118446744073709551614),
        vec2(0.118446744073709551614, 0.314159265358979323846264)))*0.141421356237309504880169);
}

vec2 cr(float t)
{
    vec2 G1 = vec2(mps[0], mps[1]);
    vec2 G2 = vec2(mps[2], mps[3]);
    vec2 G3 = vec2(mps[4], mps[5]);
    vec2 G4 = vec2(mps[6], mps[7]);
    vec2 A = G1*-0.5+G2*1.5+G3*-1.5+G4*0.5;
    vec2 B = G1+G2*-2.5+G3*2.+G4*-.5;
    vec2 C = G1*-0.5+G3*0.5 ;
    vec2 D = G2;
    return t*(t*(t*A+B)+C)+D;
}

void main()
{
    vec2 dir = vec2(cos(i_T), sin(i_T));
    float hd= i_dim.x/2.;
    vec2 sp=.5*(i_P+ vec2(1.0));
    float sv= texture(u_trail, bd(sp+v[13]/hd*dir+vec2(0.,v[12]/hd))).x;
    sv= max(sv, 0.000000001);
    float sd=v[0]/hd+v[2]*pow(sv,v[1])*250./hd;
    float md=v[9]/hd+v[11]*pow(sv,v[10])*250./hd;
    float sa=v[3]+v[5]*pow(sv, v[4]);
    float ra=v[6]+v[8]*pow(sv, v[7]);
    float m=texture(u_trail, bd(sp+ sd*vec2(cos(i_T), sin(i_T)))).x;
    float l=texture(u_trail, bd(sp+ sd*vec2(cos(i_T+sa), sin(i_T+sa)))).x;
    float r=texture(u_trail, bd(sp+ sd*vec2(cos(i_T-sa), sin(i_T-sa)))).x;
    float h=i_T;
    if (m>l&&m>r){}
    else if (m<l&&m<r){if (gn(i_P*1332.4324,i_T) > 0.5) h+= ra; else h-=ra;}
    else if (l<r) h-=ra; else if (l>r) h+=ra;
    vec2 nd=vec2(cos(h), sin(h));
    vec2 op=i_P+nd*md;
    const float segmentPop=0.0005;
    if (pen==1&&i_A<segmentPop){
        op=2.*cr(i_A/segmentPop)-vec2(1.);
        op+= nd*pow(gn(i_P*132.43,i_T), 1.8);
    }
    v_P = bd(op);
    v_A= fract(i_A+segmentPop);
    v_T =h;
}`;

const vsSource =
`#version 300 es\n
in vec4 aVertexPosition;
in vec2 aTexCoord;
precision highp float;

out vec2 vTexCoord;

void main() {
    gl_Position = aVertexPosition;
    vTexCoord = aTexCoord;
}`;

const drawFrag =
`#version 300 es\n
precision highp float;

in vec2 vTexCoord;

uniform vec2 uTextureSize;

out vec4 outColor;

uniform sampler2D uDrawTex;

void main() {
    vec2 uv = texture(uDrawTex, vTexCoord).rg;
    vec3 color = vec3(uv.r);
    outColor = vec4(clamp(color, 0.0, 1.0), 1.0);
}`;

const blurFrag =
`#version 300 es\n
precision highp float;

uniform vec2 uTextureSize;
uniform vec2 mouse;
uniform vec2 prevMouse;
uniform sampler2D uUpdateTex;

in vec2 vTexCoord;
out vec2 outState;

uniform float[19] v;

void main() {
    vec2 onePixel = 1.0 / uTextureSize;

    vec2 average = vec2(0.);

    float dec_x = vTexCoord.x - onePixel.x;
    float inc_x = vTexCoord.x + onePixel.x;
    float dec_y = vTexCoord.y - onePixel.y;
    float inc_y = vTexCoord.y + onePixel.y;

    average += texture(uUpdateTex, vec2(dec_x, dec_y)).rg;
    average += texture(uUpdateTex, vec2(dec_x, vTexCoord.y)).rg;
    average += texture(uUpdateTex, vec2(dec_x, inc_y)).rg;

    average += texture(uUpdateTex, vec2(vTexCoord.x, dec_y)).rg;
    average += texture(uUpdateTex, vTexCoord).rg;
    average += texture(uUpdateTex, vec2(vTexCoord.x, inc_y)).rg;

    average += texture(uUpdateTex, vec2(inc_x, dec_y)).rg;
    average += texture(uUpdateTex, vec2(inc_x, vTexCoord.y)).rg;
    average += texture(uUpdateTex, vec2(inc_x, inc_y)).rg;

    average /= 9.;

    outState = average * v[15];
}`;

const drawScreenFrag =
`#version 300 es\n
precision highp float;

in vec2 vTexCoord;
out vec4 outColor;

uniform sampler2D uDrawTex;
uniform int invert;

void main() {
    vec4 color = clamp(texture(uDrawTex, vTexCoord), 0., 1.);
    color.a = 1.0;
    if (invert == 1)
    {
        color.xyz = vec3(1.) - color.xyz;
    }
    outColor = color;
}`;

const clearScreenFrag =
`#version 300 es\n
precision highp float;

in vec2 vTexCoord;
out vec4 outColor;

uniform float[19] v;

void main() {
    outColor = vec4(0., 0., 0., v[18]);
}`;

function openFullscreen()
{
    if (document.body.requestFullscreen) {
        document.body.requestFullscreen();
    } else if (document.body.webkitRequestFullscreen) {
        document.body.webkitRequestFullscreen();
    } else if (document.body.msRequestFullscreen) {
        document.body.msRequestFullscreen();
    }
}

function closeFullscreen()
{
    if (document.exitFullscreen) {
        document.exitFullscreen();
    } else if (document.webkitExitFullscreen) {
        document.webkitExitFullscreen();
    } else if (document.msExitFullscreen) {
        document.msExitFullscreen();
    }
}

function createShader(gl, type, source)
{
    const shader = gl.createShader(type);
    gl.shaderSource(shader, source);
    gl.compileShader(shader);

    if (!gl.getShaderParameter(shader, gl.COMPILE_STATUS))
    {
        alert("Didn't compile shader. Info: " + gl.getShaderInfoLog(shader));
        gl.deleteShader(shader);
        return null;
    }

    return shader;
}

function createGLProgram(gl, shader_list, transform_feedback_varyings)
{
    let program = gl.createProgram();
    for (let i = 0; i < shader_list.length; i++)
    {
        let shader_info = shader_list[i];
        let shader = createShader(gl, shader_info.type, shader_info.source);
        gl.attachShader(program, shader);
    }

    if (transform_feedback_varyings != null)
    {
        gl.transformFeedbackVaryings(program,
            transform_feedback_varyings,
            gl.INTERLEAVED_ATTRIBS);
    }
    gl.linkProgram(program);

    if (!gl.getProgramParameter(program, gl.LINK_STATUS))
    {
        alert("Didn't link shader. Info: " + gl.getProgramInfoLog(program));
        this.gl.deleteProgram(program);
        return null;
    }
    return program;
}

function setupParticleBufferVAO(gl, buffers, vao, typeSize)
{
    gl.bindVertexArray(vao);
    for (let i = 0; i < buffers.length; i++)
    {
        gl.bindBuffer(gl.ARRAY_BUFFER, buffers[i].buffer_object);
        let offset = 0;
        for (let attrib_name in buffers[i].attribs)
        {
            if (buffers[i].attribs.hasOwnProperty(attrib_name))
            {
                let attrib_desc = buffers[i].attribs[attrib_name];
                gl.enableVertexAttribArray(attrib_desc.location);
                gl.vertexAttribPointer(attrib_desc.location, attrib_desc.num_components, attrib_desc.type, false, buffers[i].stride, offset);
                offset += attrib_desc.num_components * typeSize;
            }
        }
    }
    gl.bindVertexArray(null);
    gl.bindBuffer(gl.ARRAY_BUFFER, null);
}

function initialParticleData(num_parts)
{
    let data = [];
    for (let i = 0; i < num_parts; ++i) {
        data.push(Math.random()*2. - 1.);
        data.push(Math.random()*2. - 1.);
        data.push(i/num_parts);
        data.push(Math.random()*2.*3.14159);
    }
    return data;
}

function lerp (start, end, amount) {
  return (1 - amount) * start + amount * end;
}

function eerp (start, end, amount) {
  return Math.pow(start, (1. - amount)) * Math.pow(end, amount);;
}

function smartParamLerp(_a, _b, amount)
{
    let c = [];
    for (let i = 0; i < _a.length; ++i)
    {
        if (typeof _a[i] === 'number')
        {
            let val = 0;
            if (i === 1 || i === 4 || i === 7 || i === 11)
            {
                val = eerp(_a[i], _b[i], amount);
            }
            else if (i == 19)
            {
                val = eerp(_a[i], _b[i], Math.pow(amount, 10.));
            }
            else
            {
                val = lerp(_a[i], _b[i], amount);
            }
            c.push(val);
        }
    }
    return c;
}

function lerpArray(_a, _b, amount)
{
    let c = [];
    for (let i = 0; i < a.length; ++i)
    {
        if (typeof _a[i] === 'number')
        {
            c.push(lerp(_a[i], _b[i], amount));
        }
    }
    return c;
}

function allowDrop(ev)
{
    ev.preventDefault();
}

let pen = false;
function drop(ev)
{
    ev.preventDefault();
    const text = event.dataTransfer.getData("text");
    const arr = [1, 3, 5, 10, 18, 23, 24, 27, 28, 29, 32];
    if (text === "🥚" && arr.includes(edition - 1) && arr.includes(parameterIndex)) pen = true;
}

class Simulation
{
    createScreenFbo()
    {
        const texture = this.gl.createTexture();
        this.gl.bindTexture(this.gl.TEXTURE_2D, texture);
        if (useHalf)
        {
            this.gl.texImage2D(this.gl.TEXTURE_2D, 0, this.gl.RGBA, this.params.renderSize, this.params.renderSize,
                0, this.gl.RGBA, this.gl.UNSIGNED_BYTE, null);
        }
        else
        {
            this.gl.texImage2D(this.gl.TEXTURE_2D, 0, this.gl.RGBA32F, this.params.renderSize, this.params.renderSize,
                0, this.gl.RGBA, this.gl.FLOAT, null);
        }

        this.gl.texParameteri(this.gl.TEXTURE_2D, this.gl.TEXTURE_MIN_FILTER, this.gl.LINEAR);
        this.gl.texParameteri(this.gl.TEXTURE_2D, this.gl.TEXTURE_MAG_FILTER, this.gl.LINEAR);
        this.gl.texParameteri(this.gl.TEXTURE_2D, this.gl.TEXTURE_WRAP_S, this.gl.CLAMP_TO_EDGE);
        this.gl.texParameteri(this.gl.TEXTURE_2D, this.gl.TEXTURE_WRAP_T, this.gl.CLAMP_TO_EDGE);

        this.gl.bindTexture(this.gl.TEXTURE_2D, null);

        return texture;
    }

    constructor(gl, params)
    {
        this.gl = gl;
        this.params = params;
        this.frame = 0;

        this.lerpParams = paramsArray[parameterIndex];

        const posArr = [];
        posArr.length = 8;
        posArr.fill(-1.);
        this.mousePositions = new Float32Array(posArr);

        if (useHalf)
        {
            this.gl.getExtension("OES_texture_half_float");
            this.gl.getExtension("EXT_color_buffer_half_float");
        }
        else
        {
            this.gl.getExtension('EXT_color_buffer_float');
            this.gl.getExtension('OES_texture_float_linear');
            this.gl.getExtension('EXT_float_blend');
        }

        this.screenTexture = this.createScreenFbo();

        this.updateParticles = createGLProgram(
            this.gl,
            [
                {source: particleUpdateVert, type: this.gl.VERTEX_SHADER},
                {source: passthroughFrag, type: this.gl.FRAGMENT_SHADER},
            ],
            [
                "v_P",
                "v_A",
                "v_T",
            ]);

        this.renderParticles = createGLProgram(
            this.gl,
            [
                {source: particleRenderVert, type: this.gl.VERTEX_SHADER},
                {source: particleRenderFrag, type: this.gl.FRAGMENT_SHADER},
            ],
            null);

        this.update_attrib_locations = {
            i_P: {
                location: this.gl.getAttribLocation(this.updateParticles, "i_P"),
                num_components: 2,
                type: this.gl.FLOAT
            },
            i_A: {
                location: this.gl.getAttribLocation(this.updateParticles, "i_A"),
                num_components: 1,
                type: this.gl.FLOAT
            },
            i_T: {
                location: this.gl.getAttribLocation(this.updateParticles, "i_T"),
                num_components: 1,
                type: this.gl.FLOAT
            },
        };

        this.render_attrib_locations = {
            i_P: {
                location: this.gl.getAttribLocation(this.renderParticles, "i_P"),
                num_components: 2,
                type: this.gl.FLOAT
            }
        };

        this.vaos = [
            this.gl.createVertexArray(),
            this.gl.createVertexArray(),
            this.gl.createVertexArray(),
            this.gl.createVertexArray()
        ];
        this.buffers = [
            this.gl.createBuffer(),
            this.gl.createBuffer(),
        ];
        this.vao_desc = [
            {
                vao: this.vaos[0],
                buffers: [{
                    buffer_object: this.buffers[0],
                    stride: 4 * 4,
                    attribs: this.update_attrib_locations
                }]
            },
            {
                vao: this.vaos[1],
                buffers: [{
                    buffer_object: this.buffers[1],
                    stride: 4 * 4,
                    attribs: this.update_attrib_locations
                }]
            },
            {
                vao: this.vaos[2],
                buffers: [{
                    buffer_object: this.buffers[0],
                    stride: 4 * 4,
                    attribs: this.render_attrib_locations
                }],
            },
            {
                vao: this.vaos[3],
                buffers: [{
                    buffer_object: this.buffers[1],
                    stride: 4 * 4,
                    attribs: this.render_attrib_locations
                }],
            },
        ];

        this.params.numParticles = this.params.simSize * this.params.simSize * this.params.particleDensity;
        this.initial_data = new Float32Array(initialParticleData(this.params.numParticles));
        this.gl.bindBuffer(this.gl.ARRAY_BUFFER, this.buffers[0]);
        this.gl.bufferData(this.gl.ARRAY_BUFFER, this.initial_data, this.gl.STREAM_DRAW);
        this.gl.bindBuffer(this.gl.ARRAY_BUFFER, this.buffers[1]);
        this.gl.bufferData(this.gl.ARRAY_BUFFER, this.initial_data, this.gl.STREAM_DRAW);
        for (let i = 0; i < this.vao_desc.length; i++) {
            setupParticleBufferVAO(this.gl, this.vao_desc[i].buffers, this.vao_desc[i].vao, 4);
        }

        this.read = 0;
        this.write = 1;

        this.drawBlur= createGLProgram(
            this.gl,
            [
                {source: vsSource, type: this.gl.VERTEX_SHADER},
                {source: drawFrag, type: this.gl.FRAGMENT_SHADER},
            ],
            null);

        this.updateBlur= createGLProgram(
            this.gl,
            [
                {source: vsSource, type: this.gl.VERTEX_SHADER},
                {source: blurFrag, type: this.gl.FRAGMENT_SHADER},
            ],
            null);

        this.vao = this._initVertexArray();
        this.textures = new Array(2);
        for (let i = 0; i < this.textures.length; i++) {
            this.textures[i] = this._loadTexture(null);
        }
        this.framebuffer = this.gl.createFramebuffer();

        this.gl.enable(this.gl.BLEND);
        this.gl.blendFunc(this.gl.SRC_ALPHA, this.gl.ONE_MINUS_SRC_ALPHA);
        this.gl.clear(this.gl.COLOR_BUFFER_BIT | this.gl.DEPTH_BUFFER_BIT);
        this.gl.clearColor(0., 0., 0., .1);

        this.drawScreen = createGLProgram(
            this.gl,
            [
                {source: vsSource, type: this.gl.VERTEX_SHADER},
                {source: drawScreenFrag, type: this.gl.FRAGMENT_SHADER},
            ],
            null);

        this.clearScreen= createGLProgram(
            this.gl,
            [
                {source: vsSource, type: this.gl.VERTEX_SHADER},
                {source: clearScreenFrag, type: this.gl.FRAGMENT_SHADER},
            ],
            null);

        //this.setTexture(null);
    }

    gu(program, name)
    {
        return this.gl.getUniformLocation(program, name);
    }

    ga(program, name)
    {
        return this.gl.getAttribLocation(program, name);
    }

    updateParticlesHelper()
    {
        this.frame++;
        this.gl.enable(this.gl.BLEND);
        this.gl.clearColor(0., 0., 0., 1.);
        this.gl.clear(this.gl.COLOR_BUFFER_BIT | this.gl.DEPTH_BUFFER_BIT);
        this.gl.useProgram(this.updateParticles);

        this.gl.activeTexture(this.gl.TEXTURE0);

        this.gl.uniform1fv(this.gl.getUniformLocation(this.updateParticles, "v"), this.lerpParams);

        this.mousePositions[0] = this.params.mouse.x;
        this.mousePositions[1] = 1. - this.params.mouse.y;
        for (let i = 7; i >= 2; --i)
        {
            this.mousePositions[i] = this.mousePositions[i-2];
        }
        this.gl.uniform1fv(this.gl.getUniformLocation(this.updateParticles, "mps"), this.mousePositions);
        this.gl.uniform1i(this.gl.getUniformLocation(this.updateParticles, "frame"), this.frame);
        this.gl.uniform1i(this.gl.getUniformLocation(this.updateParticles, "pen"), (pen || interpolant) |0);

        this.gl.activeTexture(this.gl.TEXTURE1);
        this.gl.bindTexture(this.gl.TEXTURE_2D, this.textures[0]);
        this.gl.uniform1i(this.gu(this.updateParticles, "u_trail"), 1);
        this.gl.uniform2f(this.gu(this.updateParticles, "i_dim"),
            this.params.simSize, this.params.simSize);

        this.gl.bindVertexArray(this.vaos[this.read]);
        this.gl.bindBufferBase(
            this.gl.TRANSFORM_FEEDBACK_BUFFER, 0, this.buffers[this.write]);
        this.gl.enable(this.gl.RASTERIZER_DISCARD);
        this.gl.beginTransformFeedback(this.gl.POINTS);
        this.gl.drawArrays(this.gl.POINTS, 0, this.params.numParticles);
        this.gl.endTransformFeedback();
        this.gl.disable(this.gl.RASTERIZER_DISCARD);

        this.gl.bindBufferBase(this.gl.TRANSFORM_FEEDBACK_BUFFER, 0, null);
        this.gl.bindVertexArray(this.vaos[this.read + 2]);
        let tmp = this.read;
        this.read = this.write;
        this.write = tmp;
    }

    depositParticlesHelper()
    {
        this.gl.useProgram(this.renderParticles);
        this.gl.uniform1fv(this.gl.getUniformLocation(this.renderParticles, "v"), this.lerpParams);

        this.gl.uniform1i(this.gu(this.renderParticles, "deposit"), 1);
        this.gl.uniform1f(this.gu(this.renderParticles, "pointsize"), 1.);
        this.gl.uniform1f(this.gu(this.renderParticles, "dotSize"), this.lerpParams[19]);
        this.gl.bindTexture(this.gl.TEXTURE_2D, this.textures[0]);
        this.gl.bindFramebuffer(this.gl.FRAMEBUFFER, this.framebuffer);
        this.gl.framebufferTexture2D(this.gl.FRAMEBUFFER, this.gl.COLOR_ATTACHMENT0,
            this.gl.TEXTURE_2D, this.textures[0], 0);
        this.gl.bindVertexArray(this.vaos[this.read + 2]);

        this.gl.viewport(0, 0, this.params.simSize, this.params.simSize);
        this.gl.drawArrays(this.gl.POINTS, 0, this.params.numParticles);
    }

    blurHelper()
    {
        this.gl.disable(this.gl.BLEND);

        this.gl.bindVertexArray(this.vao);
        let blurIterations = Math.round(this.lerpParams[16]);
        for (let i = 0; i < blurIterations; i++) {
            this.gl.useProgram(this.updateBlur);

            this.gl.uniform1fv(this.gl.getUniformLocation(this.updateBlur, "v"), this.lerpParams);
            this.gl.bindTexture(this.gl.TEXTURE_2D, this.textures[1]);
            this.gl.bindFramebuffer(this.gl.FRAMEBUFFER, this.framebuffer);
            this.gl.framebufferTexture2D(this.gl.FRAMEBUFFER, this.gl.COLOR_ATTACHMENT0,
                this.gl.TEXTURE_2D, this.textures[1], 0);

            this.gl.activeTexture(this.gl.TEXTURE1);
            this.gl.bindTexture(this.gl.TEXTURE_2D, this.textures[0]);
            this.gl.uniform1i(this.gu(this.updateBlur, "uUpdateTex"), 1);

            this.gl.uniform2f(this.gu(this.updateBlur, "mouse"),
                this.params.mouse.x, this.params.mouse.y);
            this.gl.uniform2f(this.gu(this.updateBlur, "prevMouse"),
                this.params.prevMouseX, this.params.prevMouseY);
            this.gl.uniform2f(this.gu(this.updateBlur, "uTextureSize"),
                this.params.simSize, this.params.simSize);

            this.gl.viewport(0, 0, this.params.simSize, this.params.simSize);
            this.gl.clearColor(1.0, 0.0, 0.0, 1.0);
            this.gl.clear(this.gl.COLOR_BUFFER_BIT);
            this.gl.drawArrays(this.gl.TRIANGLE_STRIP, 0, 4);

            this.textures = [this.textures[1], this.textures[0]];
        }

        this.gl.enable(this.gl.BLEND);
    }

    drawParticlesToCanvas()
    {
        this.gl.bindVertexArray(this.vaos[this.read + 2]);
        this.gl.useProgram(this.renderParticles);

        this.gl.uniform1fv(this.gl.getUniformLocation(this.renderParticles, "v"), this.lerpParams);
        this.gl.uniform1i(this.gu(this.renderParticles, "deposit"), 0);
        this.gl.uniform1f(this.gu(this.renderParticles, "pointsize"), this.params.drawPointsize);
        this.gl.uniform1f(this.gu(this.renderParticles, "dotSize"), this.lerpParams[19]);

        this.gl.bindTexture(this.gl.TEXTURE_2D, this.screenTexture);
        this.gl.bindFramebuffer(this.gl.FRAMEBUFFER, this.framebuffer);
        this.gl.framebufferTexture2D(this.gl.FRAMEBUFFER, this.gl.COLOR_ATTACHMENT0, this.gl.TEXTURE_2D, this.screenTexture, 0);

        this.gl.enable(this.gl.BLEND);

        this.gl.viewport(0, 0, this.params.renderSize, this.params.renderSize);
        this.gl.drawArrays(this.gl.POINTS, 0, this.params.numParticles);
        this.gl.viewport(0, 0, this.gl.canvas.width, this.gl.canvas.height);
    }

    fadeScreen()
    {
        this.gl.useProgram(this.clearScreen);
        this.gl.enable(this.gl.BLEND);

        this.gl.uniform1fv(this.gl.getUniformLocation(this.clearScreen, "v"), this.lerpParams);

        this.gl.bindTexture(this.gl.TEXTURE_2D, this.screenTexture);
        this.gl.bindFramebuffer(this.gl.FRAMEBUFFER, this.framebuffer);
        this.gl.framebufferTexture2D(this.gl.FRAMEBUFFER, this.gl.COLOR_ATTACHMENT0, this.gl.TEXTURE_2D, this.screenTexture, 0);

        this.gl.viewport(0, 0, this.params.renderSize, this.params.renderSize);
        this.gl.bindVertexArray(this.vao);
        this.gl.drawArrays(this.gl.TRIANGLE_STRIP, 0, 4);
    }

    drawCanvasToScreen()
    {
        this.gl.useProgram(this.drawScreen);

        this.gl.uniform1i(this.gu(this.drawScreen, "invert"), parameterIndex == 27 || parameterIndex == 36 | 0);

        this.gl.bindFramebuffer(this.gl.FRAMEBUFFER, null);

        this.gl.activeTexture(this.gl.TEXTURE0);
        this.gl.bindTexture(this.gl.TEXTURE_2D, this.screenTexture);
        this.gl.uniform1i(this.gu(this.drawScreen, "uDrawTex"), 0);

        this.gl.viewport(0, 0, this.params.renderSize, this.params.renderSize);
        this.gl.bindVertexArray(this.vao);
        this.gl.drawArrays(this.gl.TRIANGLE_STRIP, 0, 4);

        this.gl.viewport(parseInt(this.params.renderSize), 0, this.params.renderSize, this.params.renderSize);
        this.gl.bindVertexArray(this.vao);
        this.gl.drawArrays(this.gl.TRIANGLE_STRIP, 0, 4);
    }

    draw()
    {
        this.params.lerpTime = Math.min(1., this.params.lerpTime + Math.max((1.-this.params.lerpTime)*this.params.convergenceRate, 0.001));
        this.lerpParams = smartParamLerp(this.params.pastParams, this.params.params, this.params.lerpTime);
        //const stickiness = 0.0;
        //if (!interpolant) this.lerpParams = lerpArray(this.lerpParams, paramsArray[edition-1], stickiness);

        this.updateParticlesHelper();
        this.depositParticlesHelper();
        this.blurHelper();

        if (this.params.displayParticles)
        {
            this.fadeScreen();
            this.drawParticlesToCanvas();
            this.drawCanvasToScreen();
            this.gl.viewport(0., 0., this.gl.canvas.width, this.gl.canvas.height);
        }
        else
        {
            this.gl.useProgram(this.drawBlur);

            this.gl.bindFramebuffer(this.gl.FRAMEBUFFER, null);
            this.gl.activeTexture(this.gl.TEXTURE0);
            this.gl.bindTexture(this.gl.TEXTURE_2D, this.textures[0]);
            this.gl.uniform1i(this.gu(this.drawBlur, "uDrawTex"), 0);
            this.gl.uniform2f(this.gu(this.drawBlur, "uTextureSize"), this.params.simSize, this.params.simSize);

            this.gl.clearColor(0.0, 0.0, 0.0, 1.0);
            this.gl.clear(this.gl.COLOR_BUFFER_BIT);
            this.gl.viewport(0, 0, this.gl.canvas.width, this.gl.canvas.height);
            this.gl.drawArrays(this.gl.TRIANGLE_STRIP, 0, 4);
        }

    }

    setTexture(source) {
        this.gl.bindTexture(this.gl.TEXTURE_2D, this.textures[0]);
        if (useHalf)
        {
            this.gl.texImage2D(this.gl.TEXTURE_2D, 0, this.gl.RGBA16F, this.params.simSize, this.params.simSize, 0, this.gl.RGBA, this.gl.HALF_FLOAT, null);
        }
        else
        {
            this.gl.texImage2D(this.gl.TEXTURE_2D, 0, this.gl.R32F, this.params.simSize, this.params.simSize, 0, this.gl.RED, this.gl.FLOAT, null);
        }
        this.gl.bindTexture(this.gl.TEXTURE_2D, null);
    }

    _initVertexArray() {
        const vao = this.gl.createVertexArray();
        this.gl.bindVertexArray(vao);

        const positionBuffer = this.gl.createBuffer();
        this.gl.bindBuffer(this.gl.ARRAY_BUFFER, positionBuffer);
        this.gl.bufferData(this.gl.ARRAY_BUFFER, new Float32Array([
            -1.0,  1.0,
            -1.0, -1.0,
            1.0,  1.0,
            1.0, -1.0,
        ]), this.gl.STATIC_DRAW);
        this.gl.enableVertexAttribArray(this.ga(this.drawBlur, "aVertexPosition"));
        this.gl.vertexAttribPointer(this.ga(this.drawBlur, "aVertexPosition"), 2, this.gl.FLOAT, false, 0, 0);
        this.gl.enableVertexAttribArray(this.ga(this.updateBlur, "aVertexPosition"));
        this.gl.vertexAttribPointer(this.ga(this.updateBlur, "aVertexPosition"), 2, this.gl.FLOAT, false, 0, 0);

        const texCoordBuffer = this.gl.createBuffer();
        this.gl.bindBuffer(this.gl.ARRAY_BUFFER, texCoordBuffer);
        this.gl.bufferData(this.gl.ARRAY_BUFFER, new Float32Array([
            0.0, 1.0,
            0.0, 0.0,
            1.0, 1.0,
            1.0, 0.0
        ]), this.gl.STATIC_DRAW);
        this.gl.enableVertexAttribArray(this.ga(this.drawBlur, "aTexCoord"));
        this.gl.vertexAttribPointer(this.ga(this.drawBlur, "aTexCoord"), 2, this.gl.FLOAT, false, 0, 0);
        this.gl.enableVertexAttribArray(this.ga(this.updateBlur, "aTexCoord"));
        this.gl.vertexAttribPointer(this.ga(this.updateBlur, "aTexCoord"), 2, this.gl.FLOAT, false, 0, 0);

        this.gl.bindVertexArray(null);
        return vao;
    }

    _loadTexture(source) {
        const texture = this.gl.createTexture();
        this.gl.bindTexture(this.gl.TEXTURE_2D, texture);
        if (useHalf)
        {
            this.gl.texImage2D(
                this.gl.TEXTURE_2D,
                0,
                this.gl.RGBA16F,
                this.params.simSize,
                this.params.simSize,
                0,
                this.gl.RGBA,
                this.gl.HALF_FLOAT,
                source
            );
        }
        else
        {

            this.gl.texImage2D(this.gl.TEXTURE_2D, 0, this.gl.RG32F, this.params.simSize, this.params.simSize, 0, this.gl.RG, this.gl.FLOAT, source);
        }

        this.gl.texParameteri(this.gl.TEXTURE_2D, this.gl.TEXTURE_MIN_FILTER, this.gl.LINEAR);
        this.gl.texParameteri(this.gl.TEXTURE_2D, this.gl.TEXTURE_MAG_FILTER, this.gl.LINEAR);
        this.gl.texParameteri(this.gl.TEXTURE_2D, this.gl.TEXTURE_WRAP_S, this.gl.REPEAT);
        this.gl.texParameteri(this.gl.TEXTURE_2D, this.gl.TEXTURE_WRAP_T, this.gl.REPEAT);

        this.gl.bindTexture(this.gl.TEXTURE_2D, null);

        return texture;
    }
}

let hashString = "";
function setHash(str)
{
    hashString = str;
    window.location.hash = str;
}

function setWindowHash()
{
    const nonZeroIndexed = parameterIndex + 1;
    setHash(nonZeroIndexed.toString().padStart(2, '0') + "_" + paramNames[parameterIndex]);
}

function readHash()
{
    let hash = window.location.hash.slice();
    if (hash.slice(1, 12) === "interpolant")
    {
        interpolant = true;
        let input = hash.split("_").slice(1);
        if (input.length == 3)
        {
            let interpolationValue = parseFloat(input[2]);
            interpolationValue = Math.max(0., Math.min(1., interpolationValue));
            setInterpolation(parseInt(input[0])-1, parseInt(input[1])-1, interpolationValue, false, true);
        }
        return;
    }
    interpolant = false;
    let number = hash.match(/\d/g);
    number = parseInt(number.join(""))-1;
    if (parameterIndex == number)
    {
        return;
    }
    setNewParameter(number);
}

window.onhashchange = readHash;
setWindowHash();

const canvas = document.getElementById("canvas");

let fullscreen = false;
function toggleFullscreen()
{
    if (fullscreen)
    {
        closeFullscreen();
    }
    else
    {
        openFullscreen();
    }
    fullscreen = !fullscreen;
}

function saveCanvas()
{
    const date = new Date();
    let name = "36_Points_" + hashString;
    saveCanvasName(name);
}

function saveCanvasName(name)
{
    const link = document.createElement('a');
    const date = new Date();
    link.download = name + '.png';
    link.href = canvas.toDataURL();
    link.click();
    link.remove();
}

const params = {
    'simSize': 512,
    'renderSize': 1080,
    'mouse': {x: 450., y: 450.},
    'particleDensity': 2.7,
    'numParticles': -1,
    'drawOpacity': 0.2,
    'fillOpacity': 1.0,
    'drawPointsize': 1.,
    'displayParticles': true,
    'canvasZoom': 1.0,
    'convergenceRate': .15,
    'name': "params",
    'update': true,
    'pastParams': paramsArray[parameterIndex],
    'params': paramsArray[parameterIndex],
    'lerpTime': 0.,
    'smartLerp': true,
};

const glContextAttributes = { preserveDrawingBuffer: true };
const gl = document.getElementById('canvas').getContext('webgl2', glContextAttributes);
if (!gl) alert('Requires WebGL2');

function updateDisplaySize()
{
    let mobileDisplay = false;
    if (mobileDisplay)
    {
        params.canvasZoom = window.innerWidth / params.renderSize;
        params.canvasZoom = Math.max(.5, params.canvasZoom);
    }
    else
    {
        params.canvasZoom = Math.max(window.innerHeight / params.renderSize, .5*window.innerWidth / params.renderSize);
        params.canvasZoom = Math.max(.5, params.canvasZoom);
    }
    if (window.devicePixelRatio < 1.25)
    {
        params.canvasZoom = Math.max(1., params.canvasZoom);
    }

    let str = parseInt(params.renderSize * params.canvasZoom);
    if (mobileDisplay)
    {
        canvas.style.width = str  + "px";
        canvas.style.height = str * 2 + "px";
        gl.canvas.width = params.renderSize;
        gl.canvas.height = params.renderSize*2;
        canvas.width = params.renderSize;
        canvas.height = params.renderSize*2;
    }
    else
    {
        canvas.style.width = str * 2  + "px";
        canvas.style.height = str + "px";
        gl.canvas.width = params.renderSize*2;
        gl.canvas.height = params.renderSize;
        canvas.width = params.renderSize*2;
        canvas.height = params.renderSize;
    }
}
updateDisplaySize();

let keyCodes = [49, 50, 51, 52, 53, 54, 55, 56, 57, 48,
     113, 119, 101, 114, 116, 121, 117, 105, 111, 112,
     97, 115, 100, 102, 103, 104, 106, 107, 108,
     122, 120, 99, 118, 98, 110, 109, 90,
     88, 67, 86, 66, 78, 77];

window.addEventListener("resize", updateDisplaySize);

function setRandomParameter()
{
    let randomIdx = Math.floor(Math.random() * keyCodes.length);

    if (randomIdx == parameterIndex)
    {
        return;
    }
    parameterIndex = randomIdx;

    params.pastParams = params.params;
    params.lerpTime = 0.;

    params.params = paramsArray[parameterIndex];
    params.drawOpacity = params.params[17];
    params.fillOpacity= params.params[18];

    setWindowHash();
}


function setNewParameter(index)
{
    if (index < 0 || index >= paramsArray.length)
    {
        setWindowHash();
        return;
    }

    parameterIndex = index;
    params.pastParams = sim.lerpParams;
    params.lerpTime = 0.;

    params.params = paramsArray[parameterIndex];
    params.drawOpacity = params.params[17];
    params.fillOpacity= params.params[18];

    pen = false;

    setWindowHash();

    console.log("Point " + (index + 1).toString().padStart(2, '0') + " (" + paramNames[index] + ")");
}

function setInterpolation(a, b, c, updateHash, fromHash)
{
    if (typeof(a) != "number" ||
        typeof(b) != "number" ||
        typeof(c) != "number" ||
        a < 0 ||
        a >= keyCodes.length ||
        b < 0 ||
        b >= keyCodes.length ||
        c < 0. || c > 1.)
    {
        console.log("Invalid interpolation parameters: " + a + " " + b + " " + c);
        setWindowHash();
        return;
    }

    params.pastParams = sim.lerpParams;
    params.params = lerpArray(paramsArray[a], paramsArray[b], c);
    params.lerpTime = 0.;
    if (fromHash)
    {
        console.log("Interpolation from Point " + (a + 1) + " to Point " + (b+1) + " with amount " + c);
    }

    if (updateHash)
    {
        setHash('interpolant_' + (a + 1) + "_" + (b + 1) + "_" + c);
    }
}

function setRandomInterpolant()
{
    let random1 = Math.floor(Math.random() * keyCodes.length);
    let random2 = Math.floor(Math.random() * keyCodes.length);
    let interpVal = parseFloat(Math.random().toFixed(3));
    setInterpolation(random1, random2, interpVal, true, false);
}

function processKeypress(charCode)
{
    let charStr = String.fromCharCode(charCode);
    if (charStr == "\\") {
        params.params = sim.lerpParams;
        params.lerpTime = 1.0;
        console.log("Frozen!");
    }
    else if (charStr == "U")
    {
        params.update = !params.update;
    }
    else if (charStr == "S") {
        saveCanvas();
        console.log("Save canvas");
    }
    else if (charStr == "R") {
        resetSim()
        console.log("Reset");
    }
    else if (charStr == "L") {
        console.log("Current parameters: " + this.lerpParams);
    }
    else if (charStr == " ")
    {
        toggleFullscreen()
        console.log("Toggle fullscreen");
    }
    else if (charStr == "~")
    {
        setRandomInterpolant()
    }
    else if (charStr == "+")
    {
        params.convergenceRate = Math.max(1e-15, 0.5 * params.convergenceRate);
        console.log("Convergence speed: " +  params.convergenceRate);
    }
    else if (charStr == "-")
    {
        params.convergenceRate = Math.min(1., 2. * params.convergenceRate);
        console.log("Convergence speed: " + params.convergenceRate);
    }
    else
    {
        if (frame > 1000)
        {
            if (charCode === 109 && Math.random() > 0.95) charCode = 86;
            else if (charCode === 104 && Math.random() > 0.95) charCode = 67;
            else if (charCode === 110 && Math.random() > 0.99) charCode = 90;
            else if (charCode === 97 && parameterIndex == 20) charCode = 78;
        }

        let charIndex = keyCodes.indexOf(charCode);
        setNewParameter(charIndex);
    }
}

document.onkeypress = function(event)
{
    framesSinceInput = 0;
    event = event || window.event;
    let charCode = event.keyCode || event.which;
    processKeypress(charCode);
};

let sim = new Simulation(gl, params);

function resetSim()
{
    sim = new Simulation(gl, params);
}

let framesSinceInput = 0;
let mouse = [];
document.addEventListener("pointermove", () => {
    event.preventDefault();
    let canvasPosition = canvas.getBoundingClientRect();
    params.mouse.x = 2.*((event.clientX - canvasPosition.x)/params.canvasZoom)/canvas.width;
    params.mouse.y = ((event.clientY - canvasPosition.y)/params.canvasZoom)/canvas.height;
    params.mouse.x = params.mouse.x - Math.floor(params.mouse.x);
    params.mouse.y = params.mouse.y - Math.floor(params.mouse.y);
});

function touchstarthelper()
{
    setRandomParameter();
    pen = true;
}

function touchendhelper()
{
    pen = false;
}

document.ontouchstart = touchstarthelper;
document.ontouchend = touchendhelper;
document.onmousestart = setRandomParameter;

const render = function()
{
    framesSinceInput++;
    frame++;
    if (framesSinceInput > 60*60*5 && parameterIndex != edition - 1)
    {
        console.log("Slow mode (~5 minutes of inactivity)... returning to original edition.");
        params.convergenceRate = 0.00001;
        setNewParameter(edition - 1);
        framesSinceInput = 0;
    }
    if (params.update) sim.draw();

    requestAnimationFrame(render);
}

requestAnimationFrame(render);
