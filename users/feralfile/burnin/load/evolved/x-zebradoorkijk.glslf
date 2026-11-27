/*
[formula] cos(mod(add(noise3(x,mix(c[0],y,x),cos(mod(add(noise3(x,mix(c[1],y,x),x),sub(x,mod(mod(div(x,c[2]),mul(y,tan(div(x,c[3])))),y))),y))),sub(x,c[4])),mul(mod(div(add(x,c[5]),c[6]),div(abs(add(mod(mod(div(x,c[7]),mul(y,tan(div(x,c[8])))),y),c[9])),y)),c[10])))

[frame_constant] -19.626 -54.271 25.887 1000 2000
[frame_constant] -7.765 -13.697 -2.053 1000 2000
[frame_constant] -3.657 -6.041 6.098 1000 2000
[frame_constant] 30.410 25.224 36.332 1000 2000
[frame_constant] 4.807 -57.291 56.870 1200 2900
[frame_constant] 31.049 -86.050 85.235 1200 2900
[frame_constant] -16.968 -21.427 22.348 1200 2900
[frame_constant] -1.684 -3.508 -0.094 1200 2900
[frame_constant] 24.528 -40.786 40.058 1200 2900
[frame_constant] 5.464 -16.701 18.217 1000 2000
[frame_constant] -2.707 -37.947 30.019 1000 2000

[output_range] -1.000000 1.000000
*/

precision mediump float;

//// simplex noise /////////////////////////////////////////////////////////////
//// 2- and 3-dimensional simplex noise, based on ashima/webgl-noise ///////////

vec2 mod289(vec2 x) {
	return x - floor(x*(1.0/289.0))*289.0;
}

vec3 mod289(vec3 x) {
	return x - floor(x*(1.0/289.0))*289.0;
}

vec4 mod289(vec4 x) {
	return x - floor(x*(1.0/289.0))*289.0;
}

vec3 permute(vec3 x) {
	return mod289(((x*34.0) + 1.0)*x);
}

vec4 permute(vec4 x) {
	return mod289(((x*34.0) + 1.0)*x);
}

vec4 taylorInvSqrt(vec4 r) {
	return 1.79284291400159 - 0.85373472095314*r;
}

// simplex returns 2-dimensional simplex noise in [-1.0, 1.0]
float simplex(vec2 v) {
  	const vec4 C = vec4(0.211324865405187,  // (3.0-sqrt(3.0))/6.0
		                0.366025403784439,  // 0.5*(sqrt(3.0)-1.0)
                       -0.577350269189626,  // -1.0 + 2.0 * C.x
       	                0.024390243902439); // 1.0 / 41.0
	// first corner
  	vec2 i  = floor(v + dot(v, C.yy));
  	vec2 x0 = v - i + dot(i, C.xx);

	// other corners
  	vec2 i1;
  	i1 = (x0.x > x0.y) ? vec2(1.0, 0.0) : vec2(0.0, 1.0);
  	vec4 x12 = x0.xyxy + C.xxzz;
  	x12.xy -= i1;

	// permutations
  	i = mod289(i); // avoid truncation effects in permutation
  	vec3 p = permute(permute(i.y + vec3(0.0, i1.y, 1.0)) + i.x + vec3(0.0, i1.x, 1.0));
  	vec3 m = max(0.5 - vec3(dot(x0, x0), dot(x12.xy, x12.xy), dot(x12.zw, x12.zw)), 0.0);
  	m = m*m;
  	m = m*m;

	// gradients: 41 points uniformly over a line, mapped onto a diamond.
	// the ring size 17*17 = 289 is close to a multiple of 41 (41*7 = 287)
	vec3 x = 2.0 * fract(p * C.www) - 1.0;
	vec3 h = abs(x) - 0.5;
	vec3 ox = floor(x + 0.5);
	vec3 a0 = x - ox;

	// normalize gradients implicitly by scaling m
	// approximation of: m *= inversesqrt(a0*a0 + h*h);
 	m *= 1.79284291400159 - 0.85373472095314*(a0*a0 + h*h);

	// compute final noise value at P
	vec3 g;
	g.x = a0.x*x0.x  + h.x*x0.y;
	g.yz = a0.yz*x12.xz + h.yz*x12.yw;
	return 140.0*dot(m, g);
}

// simplex returns 3-dimensional simplex noise in [-1.0, 1.0]
float simplex(vec3 v) { 
	const vec2  C = vec2(1.0/6.0, 1.0/3.0) ;
	const vec4  D = vec4(0.0, 0.5, 1.0, 2.0);

	// first corner
	vec3 i  = floor(v + dot(v, C.yyy) );
	vec3 x0 =   v - i + dot(i, C.xxx) ;

	// other corners
	vec3 g = step(x0.yzx, x0.xyz);
	vec3 l = 1.0 - g;
	vec3 i1 = min( g.xyz, l.zxy );
	vec3 i2 = max( g.xyz, l.zxy );

	vec3 x1 = x0 - i1 + C.xxx;
	vec3 x2 = x0 - i2 + C.yyy; // 2.0*C.x = 1/3 = C.y
	vec3 x3 = x0 - D.yyy;      // -1.0+3.0*C.x = -0.5 = -D.y

	// permutations
	i = mod289(i); 
	vec4 p = permute(permute(permute( 
	         i.z + vec4(0.0, i1.z, i2.z, 1.0 ))
	       + i.y + vec4(0.0, i1.y, i2.y, 1.0 )) 
	       + i.x + vec4(0.0, i1.x, i2.x, 1.0 ));

	// gradients: 7x7 points over a square, mapped onto an octahedron.
	// the ring size 17*17 = 289 is close to a multiple of 49 (49*6 = 294)
	float n_ = 0.142857142857; // 1.0/7.0
	vec3  ns = n_*D.wyz - D.xzx;

	vec4 j = p - 49.0*floor(p*ns.z*ns.z);  //  mod(p,7*7)

	vec4 x_ = floor(j*ns.z);
	vec4 y_ = floor(j - 7.0*x_);    // mod(j,N)

	vec4 x = x_ *ns.x + ns.yyyy;
	vec4 y = y_ *ns.x + ns.yyyy;
	vec4 h = 1.0 - abs(x) - abs(y);

	vec4 b0 = vec4(x.xy, y.xy);
	vec4 b1 = vec4(x.zw, y.zw);

	vec4 s0 = floor(b0)*2.0 + 1.0;
	vec4 s1 = floor(b1)*2.0 + 1.0;
	vec4 sh = -step(h, vec4(0.0));

	vec4 a0 = b0.xzyw + s0.xzyw*sh.xxyy;
	vec4 a1 = b1.xzyw + s1.xzyw*sh.zzww;

	vec3 p0 = vec3(a0.xy, h.x);
	vec3 p1 = vec3(a0.zw, h.y);
	vec3 p2 = vec3(a1.xy, h.z);
	vec3 p3 = vec3(a1.zw, h.w);

	// normalize gradients
	vec4 norm = taylorInvSqrt(vec4(dot(p0,p0), dot(p1,p1), dot(p2, p2), dot(p3,p3)));
	p0 *= norm.x;
	p1 *= norm.y;
	p2 *= norm.z;
	p3 *= norm.w;

	// mix final noise value
	vec4 m = max(0.6 - vec4(dot(x0,x0), dot(x1,x1), dot(x2,x2), dot(x3,x3)), 0.0);
	m = m*m;
	return 56.0*dot(m*m, vec4(dot(p0,x0), dot(p1,x1), dot(p2,x2), dot(p3,x3)));
}

//// low-level utilities ///////////////////////////////////////////////////////

// Approximate isinf() by treating inf as just a very big number.
bool IsInf(float a) {
	if (a < -3.4e38) {
		return true;
	} else if (a > 3.4e38) {
		return true;
	} else {
		return false;
	}
}

////////////////////////////////////////////////////////////////////////////////
// expression tree operators                                                  // 
////////////////////////////////////////////////////////////////////////////////

//// unary building block operators ////////////////////////////////////////////

float Abs(float a) {
	if (a < 0.0) {
		return -a;
	}
	return a;
}

float Neg(float a) {
	return -a;
}

float Sign(float a) {
	if (a < 0.0) {
		return -1.0;
	} else {
		return 1.0;
	}
}

float Floor(float a) {
	return floor(a);
}

float Ceil(float a) {
	return ceil(a);
}

float Fract(float a) {
	return fract(a);
}

float Sin(float a) {
	return sin(a);
}

float Cos(float a) {
	return cos(a);
}

float Tan(float a) {
	return tan(a);
}

float Asin(float a) {
	if (a < -1.0 || a > 1.0) {
		return 0.0;
	}
	return asin(a);
}

float Acos(float a) {
	if (a < -1.0 || a > 1.0) {
		return 0.0;
	}
	return acos(a);
}

float Atan(float a) {
	return atan(a);
}

float Sqrt(float a) {
	if (a < 0.0) {
		return 0.0;
	}
	return sqrt(a);
}

float Log(float a) {
	if (a < 0.0) {
		return 0.0;
	}
	return log(a);
}

float Sigma(float a) {
	return (a / (1.0 + abs(a)));
}

float Triangle(float a) {
	if (a < 0.0) {
		a = -a;
	}
	return abs(mod(a, 4.0) - 2.0) - 1.0;
}

float Square(float a) {
	return 2.0*(2.0*floor(a) - floor(2.0*a)) + 1.0;
}

float Infz(float a) {
	if (IsInf(a)) {
		return 0.0;
	} else {
		return a;
	}
}

float Noise1(float a) {
	return simplex(vec2(0.4*a, 0.0));
}

//// binary building block operators ///////////////////////////////////////////

float Add(float a, float b) {
	return a + b;
}

float Sub(float a, float b) {
	return a - b;
}

float Mul(float a, float b) {
	return a * b;
}

float Div(float a, float b) {
	if (a == 0.0 && b == 0.0) {
		return 0.0;
	}
	return a / b;
}

float Mod(float a, float b) {
	if (b == 0.0) {
		return 0.0;
	}

	float q = a/b;
	if (q < 0.0) {
		return (a - b*ceil(q));
	} else {
		return (a - b*floor(q));
	}
}

float Min(float a, float b) {
	if (a < b) {
		return a;
	}
	return b;
}

float Max(float a, float b) {
	if (a > b) {
		return a;
	}
	return b;
}

float Pow(float a, float b) {
	return pow(a, b);
}

float Clamplo(float a, float b) {
	if (a < b) {
		return b;
	} else {
		return a;
	}
}

float Clamphi(float a, float b) {
	if (a > b) {
		return b;
	} else {
		return a;
	}
}

float And(float a, float b) {
	if ((a > 0.0) && (b > 0.0)) {
		return 1.0;
	} else {
		return -1.0;
	}
}

float Or(float a, float b) {
	if ((a > 0.0) || (b > 0.0)) {
		return 1.0;
	} else {
		return -1.0;
	}
}

float Xor(float a, float b) {
	if ((a > 0.0) != (b > 0.0)) {
		return 1.0;
	} else {
		return -1.0;
	}
}

float Noise2(float a, float b) {
	return simplex(vec2(a, b));
}

//// ternary building block operators //////////////////////////////////////////

float Clamp(float a, float b, float c) {
	if (a < b) {
		return b;
	} else if (a > c) {
		return c;
	} else {
		return a;
	}
}

float Mix(float a, float b, float c) {
	return mix(a, b, c);
}

float Izero(float a, float b, float c) {
	if ((a > b) && (a < c)) {
		return 0.0;
	} else {
		return a;
	}
}

float Ozero(float a, float b, float c) {
	if ((a < b) || (a > c)) {
		return 0.0;
	} else {
		return a;
	}
}

float Ifgtz(float a, float b, float c) {
	if (a > 0.0) {
		return b;
	} else {
		return c;
	}
}

float Ifltz(float a, float b, float c) {
	if (a < 0.0) {
		return b;
	} else {
		return c;
	}
}

float Ifeqz(float a, float b, float c) {
	if (a == 0.0) {
		return b;
	} else {
		return c;
	}
}

float Noise3(float a, float b, float c) {
	return simplex(vec3(a, b, c));
}


//// main program: evalutate the formula at the current pixel //////////////////  

uniform vec4 u_view;    // viewport size and xy scale
uniform vec2 u_luma;    // formula luminance offset and scale
uniform float C[11];    // frame constants

void main() {
    // position the origin in the center
    float v0 = (gl_FragCoord.x / u_view[0]) - 0.5;
    float v1 = (gl_FragCoord.y / u_view[1]) - 0.5;
    // correct x and y axes for aspect ratio
    v0 *= u_view[2];
    v1 *= u_view[3];

	float shade =
		Cos(Mod(Add(Noise3(v0,Mix(C[0],v1,v0),Cos(Mod(Add(Noise3(v0,Mix(C[1],v1,v0),v0),Sub(v0,Mod(Mod(Div(v0,C[2]),Mul(v1,Tan(Div(v0,C[3])))),v1))),v1))),Sub(v0,C[4])),Mul(Mod(Div(Add(v0,C[5]),C[6]),Div(Abs(Add(Mod(Mod(Div(v0,C[7]),Mul(v1,Tan(Div(v0,C[8])))),v1),C[9])),v1)),C[10])));

    // output luminance correction 
    shade = u_luma[1]*(shade - u_luma[0]);

	gl_FragColor = vec4(shade, shade, shade, 1.0);
}
