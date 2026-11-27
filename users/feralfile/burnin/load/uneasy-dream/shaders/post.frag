#ifdef GL_ES
precision highp float;
#endif

varying vec2 vTexCoord;
uniform sampler2D tex0;
uniform float time;
uniform float osc;
uniform float vel;
uniform float amp;

vec3 hsl2rgb(vec3 c) {
    vec4 K = vec4(1.0, 2.0 / 3.0, 1.0 / 3.0, 3.0);
    vec3 p = abs(fract(c.xxx + K.xyz) * 6.0 - K.www);
    return c.z * mix(K.xxx, clamp(p - K.xxx, 0.0, 1.0), c.y);
}

# define gamma 2.2

vec3 rgb2hsl(vec3 color) {
    vec3 hsl;
    float fmin = min(min(color.r, color.g), color.b);
    float fmax = max(max(color.r, color.g), color.b);
    float delta = fmax - fmin;

    hsl.z = (fmax + fmin) / 2.0;

    if (delta == 0.0){
        hsl.x = 0.0;
        hsl.y = 0.0;
    } 
    else {
        if (hsl.z < 0.5)
            hsl.y = delta / (fmax + fmin);
        else
            hsl.y = delta / (2.0 - fmax - fmin);

        float deltaR = (((fmax - color.r) / 6.0) + (delta / 2.0)) / delta;
        float deltaG = (((fmax - color.g) / 6.0) + (delta / 2.0)) / delta;
        float deltaB = (((fmax - color.b) / 6.0) + (delta / 2.0)) / delta;

        if (color.r == fmax)
            hsl.x = deltaB - deltaG;
        else if (color.g == fmax)
            hsl.x = (1.0 / 3.0) + deltaR - deltaB;
        else if (color.b == fmax)
            hsl.x = (2.0 / 3.0) + deltaG - deltaR;

        if (hsl.x < 0.0)
            hsl.x += 1.0;
        else if (hsl.x > 1.0)
            hsl.x -= 1.0;
    }

    return hsl;
}

void main() {

  vec2 st = vTexCoord;
  
  float vt = time*vel;
  st.x += cos(st.y*osc+vt)*amp;
  st.y += cos(st.x*osc+vt)*amp;
  st.x += cos(st.y*osc+vt)*amp;
  st.y += cos(st.x*osc+vt)*amp;
  
  vec3 col = texture2D(tex0, st).rgb*vec3(1.2936, 1.2642, 1.323);

  col = pow(col, vec3(1.0/gamma));

  col = rgb2hsl(col);
  col = hsl2rgb(col+vec3(0.0, 0.1, 0.16));  
  col.r = pow(col.r, 1.1);
  col.g = pow(col.g, 1.3);

  col = pow(col, vec3(gamma));

  gl_FragColor = vec4(col.rgb, 1.0);
}
