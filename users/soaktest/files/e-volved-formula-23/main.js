"use strict"

import {Variator} from "./variator.js"


//// config ////////////////////////////////////////////////////////////////////

const shader_name = "x-zebradoorkijk.glslf"

// start a formula with random frame constants or not?
const startRandom = true

// field of view: the larger the value, the more 'wide-angle'
const fov = 75.0

// title fading timings
const fade_t = 60
const title_t = 4*60
const titling_t = title_t + 4*fade_t


//// globals ///////////////////////////////////////////////////////////////////

const canvas = document.getElementById("glcanvas")
const overlay = document.getElementById("overlay")

const gl = canvas.getContext("webgl")

var shaderProgram = null
var programInfo = null  // 'bindings' to the shader program
var buffers = null      // GL render data

var uni = {
    config: null, // formula configuration 
    C: null,      // Float32Array of frame constant uniform values
    vari: [],     // a variator for each frame constant
}

var tick = 0

var luma = {
    offset: 0.0,
    scale: 1.0,
}


//// high level functionality //////////////////////////////////////////////////

setSize()
load()


function load() {
    const vsPromise = fetch("vertex.glslv")
    .then(result => result.text())
    const fsPromise = fetch(shader_name)
    .then(result => result.text()) 

    Promise.all([vsPromise, fsPromise])
    .then((sources) => {init(sources)})
}


function init(sources) {
    if (gl == null) {
        alert('No WebGL available')
        return
    }

    uni.config = extractConfig(sources[1])
    console.log("formula", uni.config.formula)
    console.log("uniform ranges", uni.config.c_ranges)
    console.log("output range", uni.config.out_range)

    // compute luma offset and scale
    luma.offset = uni.config.out_range.min
    luma.scale = 1.0/(uni.config.out_range.max - uni.config.out_range.min)
    // add some leeway
    luma.offset *= 1.01
    luma.scale  *= 0.99

    const n = uni.config.c_ranges.length
    uni.C = new Float32Array(n)
    for (let i = 0; i < n; i++) {
        let v = uni.config.c_ranges[i]
        let value = v.val
        if (startRandom == true) {
            value = randomInRange(v.min, v.max)
            v.val = value
        }
        uni.C[i] = value
        uni.vari.push(new Variator(value, v.min, v.max, v.imin, v.imax))
    }

    /*
    // replace frame constants in the formula by actual values
    for (let i = 0; i < n; i++) {
        const pattern = `c[${i}]` 
        const value = uni.config.c_ranges[i].val.toFixed(3)
        //console.log("replacing constant", i, pattern, value)
        uni.config.formula = uni.config.formula.replaceAll(pattern, value)
    }
    */
    // replace frame constants format in the formula to more readable names: 
    // e.g. c[0] -> p1, c[1] -> p2, ..
    for (let i = 0; i < n; i++) {
        const pattern = `c[${i}]` 
        const new_name = `p${i+1}`
        uni.config.formula = uni.config.formula.replaceAll(pattern, new_name)
    }

    // add spaces to the formula string so it can be split into multiple lines
    uni.config.formula = uni.config.formula.replace(/,/g, ', ')
    // add spaces before each opening ( so it can be split better
    uni.config.formula = uni.config.formula.replaceAll('(', ' (')

    //console.log(`vsSource:\n${sources[0]}`)
    //console.log(`fsSource:\n${sources[1]}`)
    shaderProgram = createShaderProgram(gl, sources[0], sources[1])
    buffers = initBuffers(gl)

    programInfo = {
        program: shaderProgram,
        uniforms: {
            view: gl.getUniformLocation(shaderProgram, "u_view"),
            luma: gl.getUniformLocation(shaderProgram, "u_luma"),
            C: gl.getUniformLocation(shaderProgram, "C"),
        },
        attribs: {
            vtx_pos: gl.getAttribLocation(shaderProgram, "a_vtxPos"),
        },
    }
    console.log(programInfo)
    
    setSize()
    window.addEventListener("resize", setSize)
    window.requestAnimationFrame(draw)
}


function setSize() {
	const w = window.innerWidth
	const h = window.innerHeight

	canvas.width = w
	canvas.height = h
	gl.viewport(0, 0, w, h)

    // adapt font size
    let fs = Math.sqrt(w*h) / 32.0
    /*
    // extra small fonts for very long formulae
    if (uni.config.formula.length > 1000) {
        fs /= Math.sqrt((uni.config.formula.length/1000.0))
    }
    */
    overlay.style.fontSize = `${fs}px`
}


function titling() {
    if (tick < fade_t) {
        // fading in the formula text
        let a = tick / fade_t
        overlay.style.color = `rgba(255, 255, 255, ${a})`
    } else if (tick < fade_t + title_t) {
        // do nothing (show title)
    } else if (tick < title_t + 2*fade_t) {
        // fading out the title
        let a = 1.0 - (tick - (fade_t + title_t)) / fade_t
        overlay.style.color = `rgba(255, 255, 255, ${a})`
    } else if (tick < title_t + 3*fade_t) {
        // do nothing (black) 
    } else if (tick < titling_t) {
        // fade out the overlay
        let a = 1.0 - (tick - (3*fade_t + title_t)) / fade_t
        overlay.style.backgroundColor = `rgba(0, 0, 0, ${a})`
        render()
    } else {
        // stop displaying the title element
        overlay.style.display = "none"
    }
}


function draw() {
	window.requestAnimationFrame(draw)
    if (tick <= titling_t) {
        titling()
    } else {
        render()
    }

    overlay.innerHTML = uni.config.formula
	tick++
}


function render() {
	gl.clearColor(0.5, 0.5, 0.5, 1.0)
    gl.clear(gl.COLOR_BUFFER_BIT)

    // update frame constants
    for (let i = 0; i < uni.C.length; i++) {
        uni.C[i] = uni.vari[i].nextValue()
    }

	gl.useProgram(programInfo.program)
    setPositionAttribute(gl, buffers, programInfo)

    // set viewport size and scaling (handle horizontal/vertical)
    const vw = canvas.width
    const vh = canvas.height
    const x_scale = vw > vh ? vw/vh : 1.0
    const y_scale = vw < vh ? vh/vw : 1.0
    gl.uniform4f(programInfo.uniforms.view, vw, vh, fov*x_scale, fov*y_scale)

    // set luma correction
    gl.uniform2f(programInfo.uniforms.luma, luma.offset, luma.scale)

    // set the frame constants 
    gl.uniform1fv(programInfo.uniforms.C, uni.C)

	gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4)
}


//// GLSL utilities ////////////////////////////////////////////////////////////

function createShaderProgram(gl, vsSource, fsSource) {
	const vertexShader = compileShader(gl, gl.VERTEX_SHADER, vsSource)
	const fragmentShader = compileShader(gl, gl.FRAGMENT_SHADER, fsSource)

	const program = gl.createProgram()
	gl.attachShader(program, vertexShader)
	gl.attachShader(program, fragmentShader)
	gl.linkProgram(program)

	if (!gl.getProgramParameter(program, gl.LINK_STATUS)) {
		let log = gl.getProgramInfoLog(program)
	    alert(`gl.linkProgram() error: ${log}`)
	    return null
	}

	return program
}


function compileShader(gl, type, source) {
    const shader = gl.createShader(type)
    gl.shaderSource(shader, source)
    gl.compileShader(shader)

    if (!gl.getShaderParameter(shader, gl.COMPILE_STATUS)) {
    	let log = gl.getShaderInfoLog(shader)
        alert(`gl.compileShader() error: ${log}`)
        gl.deleteShader(shader)
        return null
    }

    return shader
}


function initBuffers(gl) {
    return {
        position: initPositionBuffer(gl),
    }
}


function initPositionBuffer(gl) {
    // full size plane
    const vtx_pos = [
    	 1.0,  1.0,
    	-1.0,  1.0, 
    	 1.0, -1.0, 
    	-1.0, -1.0
    ]

    const positionBuffer = gl.createBuffer()
    gl.bindBuffer(gl.ARRAY_BUFFER, positionBuffer)
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array(vtx_pos), gl.STATIC_DRAW)
    
    return positionBuffer
}


function setPositionAttribute(gl, buffers, programInfo) {
    const numComponents = 2
    const type = gl.FLOAT
    const normalize = false
    const stride = 0
    const offset = 0
    gl.bindBuffer(gl.ARRAY_BUFFER, buffers.position)
    gl.vertexAttribPointer(
        programInfo.attribs.position,
        numComponents,
        type,
        normalize,
        stride,
        offset,
    )
    gl.enableVertexAttribArray(programInfo.attribs.position)
}


//// utilities /////////////////////////////////////////////////////////////////

function randomInRange(min, max) {
    return min + Math.random()*(max - min)
}


function extractConfig(source) {
    // find first comment block
    const start = source.indexOf("/*") + 2
    const stop = source.indexOf("*/")
    const comment = source.substring(start, stop)
    const lines = comment.split('\n')

    let result = {
        formula: "",
        c_ranges: [],
        out_range: null,
    }
    for (let i = 0; i < lines.length; i++) {
        //console.log(`l[${i}]: ${lines[i]}`)
        let w = lines[i].split(' ')
        switch (w[0]) {
        case '[formula]':
            result.formula = lines[i].slice(9)
            break
        case '[frame_constant]': 
            result.c_ranges.push({
                val: parseFloat(w[1]),
                min: parseFloat(w[2]), 
                max: parseFloat(w[3]), 
                imin: parseInt(w[4]), 
                imax: parseInt(w[5]),
            })
            break
        case '[output_range]':
            result.out_range = {
                min: parseFloat(w[1]),
                max: parseFloat(w[2]),
            }
            break
        }
    }
    return result
}
