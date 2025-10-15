
let aboutText = `
▄████████    ▄████████    ▄████████  ▄█     █▄   ▄█       
███    ███   ███    ███   ███    ███ ███     ███ ███       
███    █▀    ███    ███   ███    ███ ███     ███ ███       
███         ▄███▄▄▄▄██▀   ███    ███ ███     ███ ███       
███        ▀▀███▀▀▀▀▀   ▀███████████ ███     ███ ███       
███    █▄  ▀███████████   ███    ███ ███     ███ ███       
███    ███   ███    ███   ███    ███ ███ ▄█▄ ███ ███▌    ▄ 
████████▀    ███    ███   ███    █▀   ▀███▀███▀  █████▄▄██ 
             ███    ███                          ▀         


CRAWL BY TRAVESS SMALLEY, 2024
A SERIES OF 512 MAPS
DESIGNED TO BE 
EXPLORED & CONNECTED.

CONTROLLED WITH ARROW KEYS, WASD, OR CLICK/TOUCH.

GREEN = SPAWN
RED = TOWER
BLUE = WARP
YELLOW = DOOR
CYAN = ASCEND MULTI LEVEL
MAGENTA = DESCEND MULTI LEVEL

SHIFT = PRECISION CONTROL

MADE FOR THE EXHIBITION 
CRAWL, ON FERAL FILE
CURATED BY CASEY REAS.

MAP VIEWER CREATED IN P5.JS
VERSION: 1.0.81
07/25/2024 12:00PM
`;

let img, imgX, imgY;
let mapImages = [];
let mapNames = [];
let mapSeeds = [];
let fogs = [];
let visitationGrids = [];
let playerPathLayers = [];
let unifont;

let currentLevel = 0; 

let initialZoomLevel = 4;
let zoomLevels = [];
let zoomLevel = 4;
let previousZoomLevel = null;
let zoomResetTime = 0; 

let keys = {};
let shiftHeld = false;
let mouseHeld = false;
let mouseXDirection = 0;
let mouseYDirection = 0;

let normalSpeed = 3;
let slowSpeed = 0.3;
let warpSpeed = false;
let originalNormalSpeed = normalSpeed;
let originalSlowSpeed = slowSpeed;
let originalZoomLevel = zoomLevel;
let maxZoomOutLevels = [];

let onBlack = false;

let blueStartTime = 0; 
let onBlue = false; 

let lastMoveTime = 0; 
let autoExplore = false; 
let autoExploreDirection = -1; 
let directionChangeCooldown = 0; 

let magentaStartTime = 0;
let onMagenta = false;
let magentaLanded = [];
let cyanStartTime = 0;
let onCyan = false;
let levelChangeCooldown = 500; 
let lastLevelChangeTime = 0;
let highestLevelReached = 0;

let outOfBoundsStartTime = 0;
let isInBounds = true;
let lastInBoundsPosition = { x: 0, y: 0 };

let gridSize = 32; 
let gridWidth = Math.ceil(2560 / gridSize);
let gridHeight = Math.ceil(2560 / gridSize); 
let visitGrid = Array.from({ length: gridHeight }, () => Array(gridWidth).fill(false));
let lastUpdatedCell = { x: null, y: null };

let wormButtonVisible = false;
let wormButtonVisibles = [];
let wormAllowed = [];
let wormButtonCooldown = 0;
let lastWormTime = 0;
const wormCooldownTime = 5000;

let warpButtonVisibles = [];
let warpAllowed = [];
let warpButtonCooldown = 0;
let lastWarpTime = 0;
const warpCooldownTime = 5000; 

let spawns = []; 
let spawnAllowed = [];
let spawnButtonCooldown = 0;
let lastSpawnTime = 0;
const spawnCooldownTime = 1000;

let showDebug = false;
let showGrid = false;

let padding = 10;
let iconSize = 32;
let miniMapActive = false;
let precisionModeActive = false;
let touchPressedState = false;
let hoveredIcon = null;
let buttonManager;
let warpButtonVisible = false;

let patternArrayChecker = [
    [0,1,0,1],
    [1,0,1,0],
    [0,1,0,1],
    [1,0,1,0]
]

let patternArraySlash = [
    [1,0,0,0,1],
    [0,1,0,1,0],
    [0,0,1,0,0],
    [0,1,0,1,0],
    [1,0,0,0,1],
]

let showOverlay = false;

/*
###############
p5.js FUNCTIONS
###############
*/

function preload() {
    let promises = [];

    let mapFilesPromise = fetch('maps/maps.json')
        .then(response => response.json())
        .then(files => {

            files.forEach((file, index) => {
                promises.push(new Promise(resolve => {
                    let img = loadImage(`maps/${file}`, () => {
                        mapNames[index] = extractMapInfo(file, 1);
                        mapSeeds[index] = extractMapInfo(file, 8);
                        resolve(img); // Make sure to resolve the img
                    });
                    mapImages[index] = img;
                }));
            });
        })
        .catch(error => console.error('Error loading map files:', error));

    promises.push(mapFilesPromise);


    let fontPromise = new Promise(resolve => {
        unifont = loadFont('unifont-15.1.05.otf', resolve);
    });

    promises.push(fontPromise);

    return Promise.all(promises);
}


function setup() {
    createCanvas(windowWidth, windowHeight);
    noSmooth();
    textFont(unifont);
    
    mapImages.forEach((img, index) => {
        if (!fogs[index]) {
            let newFog = createGraphics(img.width, img.height);
            newFog.pixelDensity(0.1);
            newFog.fill(120, 120, 120, 255);
            newFog.noStroke();
            newFog.rect(0, 0, newFog.width, newFog.height);
            fogs[index] = newFog;
        }

        if (!visitationGrids[index]) {
            let newVisitationGrid = Array.from({ length: gridHeight }, () => Array(gridWidth).fill(false));
            visitationGrids[index] = newVisitationGrid;
        }

        if (!playerPathLayers[index]) {
            //console.log("Creating new player path layer for level", index);
            let newPathLayer = createGraphics(img.width, img.height);
            newPathLayer.pixelDensity(1);
            newPathLayer.clear();
            playerPathLayers[index] = newPathLayer;
        }
    });

    buttonManager = new ButtonManager(); // Initialize buttonManager here

    buttonManager.addButton(padding, 2 * padding + iconSize, iconSize, 'yellow', 'lime', 'precisionMode', togglePrecisionMode, patternArrayChecker); 
    buttonManager.addButton(padding, 3 * padding + 2 * iconSize, iconSize, 'green', 'lightgreen', 'zoomIn', zoomIn);
    buttonManager.addButton(padding, 4 * padding + 3 * iconSize, iconSize, 'red', 'lightred', 'zoomOut', zoomOut);
    
    // DESKTOP CONDITIONS
    if (!isMobile()) {
        buttonManager.addButton(padding, padding, iconSize, 'white', 'black', 'miniMap', gridToggle);
        showGrid = true;
        updateButtonState('miniMap', gridToggle);
        buttonManager.addButton(padding, 10 * padding + 9 * iconSize, iconSize, 'black', 'white', 'debugToggle', debugToggle);
        buttonManager.addButton(padding, 11 * padding + 10 * iconSize, iconSize, 'white', 'lime', 'saveComposite', saveCompositeImage);
    }

    currentLevel = 0;
    highestLevelReached = 0;

    img = mapImages[currentLevel];
    fog = fogs[currentLevel];
    visitGrid = visitationGrids[currentLevel];
    worm = null;

    setupLevel(); 

    let validStart = false;
    while (!validStart) {
        imgX = random(0, img.width);
        imgY = random(0, img.height);
        let pixelColor = img.get(imgX, imgY);

        if (!(pixelColor[0] === 0 && pixelColor[1] === 0 && pixelColor[2] === 0) && 
            !(pixelColor[0] === 255 && pixelColor[1] === 0 && pixelColor[2] === 255) && 
            !(pixelColor[0] === 0 && pixelColor[1] === 255 && pixelColor[2] === 255) && 
            !(pixelColor[0] === 0 && pixelColor[1] === 0 && pixelColor[2] === 255)) {   
            validStart = true;
        }
    }

    lastMoveTime = millis();

    window.addEventListener('keydown', function(e) {
        if (['ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight'].includes(e.code)) {
            e.preventDefault();
        }
    }, false);

    document.addEventListener('gesturestart', function(e) {
        e.preventDefault();
    });

    let canvasElement = document.querySelector('canvas');
    if (canvasElement) {
        canvasElement.addEventListener('touchstart', function(e) {
            e.preventDefault();
        }, { passive: false });
        canvasElement.addEventListener('touchmove', function(e) {
            e.preventDefault();
        }, { passive: false });
        canvasElement.addEventListener('touchend', function(e) {
            e.preventDefault();
        }, { passive: false });
    }
}

function draw() {
    clear(); 
    background(0); 

    let pixelColor = img.get(imgX, imgY);

    if (previousZoomLevel !== null && millis() > zoomResetTime) {
        zoomLevel = previousZoomLevel;
        previousZoomLevel = null;
    }

    let viewWidth = width / zoomLevel;
    let viewHeight = height / zoomLevel;
    let viewX = imgX - viewWidth / 2;
    let viewY = imgY - viewHeight / 2;

    let sx = max(0, viewX);
    let sy = max(0, viewY);

    let sWidth = (viewX < 0) ? viewWidth + viewX : min(viewWidth, img.width - sx);
    let sHeight = (viewY < 0) ? viewHeight + viewY : min(viewHeight, img.height - sy);

    let dx = (viewX < 0) ? -viewX * zoomLevel : 0;
    let dy = (viewY < 0) ? -viewY * zoomLevel : 0;

    let dWidth = (sWidth + sx <= img.width) ? sWidth * zoomLevel : (img.width - sx) * zoomLevel;
    let dHeight = (sHeight + sy <= img.height) ? sHeight * zoomLevel : (img.height - sy) * zoomLevel;

    image(img, dx, dy, dWidth, dHeight, sx, sy, sWidth, sHeight);

    clearFog(pixelColor);
    
    image(fog, dx, dy, dWidth, dHeight, sx, sy, sWidth, sHeight);

    image(playerPathLayer, dx, dy, dWidth, dHeight, sx, sy, sWidth, sHeight);

    // DRAW SPAWN
    spawns.forEach(spawn => {
        spawn.update();
        spawn.draw();
    });

    // DRAW WORM
    if (worm) { 
        worm.update();
        if (!worm.alive) {
            worm = null; 
        } else {
            worm.draw(viewX, viewY, zoomLevel);
        }
    }

    // DRAW PLAYER
    let playerColor;
    if (onMagenta || onCyan || onBlue) {
        playerColor = color(255, 255, 0); 
    } else if (onBlack) {
        playerColor = color(255, 255, 255); 
    } else {
        playerColor = color(255, 0, 255);
    }
    fill(playerColor);
    rectMode(CENTER);
    noStroke();
    let playerSize = 5 * zoomLevel; // Size of the player square
    rect(width / 2, height / 2, playerSize, playerSize);

    if(precisionModeActive){
        fill(0,255,0)
        rect(width / 2, height / 2, playerSize/2, playerSize/2);
    }


    let currentMillis = millis();
    if (!shiftHeld && currentMillis - lastMoveTime > 500 && !autoExplore) {
        autoExplore = true;
    } else if (shiftHeld) {
        lastMoveTime = millis();  
    }

    updateMovement(pixelColor);
    checkBounds();

    if (showDebug) {
        displayDebug(pixelColor);
    }

    if (showGrid) {
        drawExplorationGrid();
    }

    // DRAW BUTTONS
    buttonManager.drawButtons();
    if (hoveredIcon) {
        fill(255);
        textSize(16);
        textAlign(LEFT, TOP);
        let tooltipText;
        switch (hoveredIcon) {
            case 'miniMap':
                tooltipText = 'Map (G)';
                break;
            case 'precisionMode':
                tooltipText = 'Precision Mode (Shift)';
                break;
            case 'zoomIn':
                tooltipText = 'Zoom In (+)';
                break;
            case 'zoomOut':
                tooltipText = 'Zoom Out (-)';
                break;
            case 'spawn':
                tooltipText = 'Spawn (R)';
                break;
            case 'warp':
                tooltipText = 'Warp (T)';
                break;
            case 'createWorm':
                tooltipText = 'Worm (U)';
                break;
            case 'previousLevel':
                tooltipText = 'Previous Level ( [ )';
                break;
            case 'nextLevel':
                tooltipText = 'Next Level ( ] )';
                break;
            case 'debugToggle':
                tooltipText = 'More Info (?)';
                break;
            case 'saveComposite':
                tooltipText = 'Save Image (P)';
                break;
            default:
                tooltipText = '';
        }
        text(tooltipText, 60, 18);
    }
    drawOverlay();
}

function windowResized() {
    resizeCanvas(windowWidth, windowHeight);
}

/*
##############
EVENT HANDLERS
##############
*/

// KEYBOARD EVENTS
function keyPressed() {
    keys[key.toLowerCase()] = true;
    if (keyCode === SHIFT) {
        shiftHeld = true;
        autoExplore = false;
        lastMoveTime = millis(); 
        precisionModeActive = true; 
        updateButtonState('precisionMode', true);
    }
    if (key === '+' || key === '=') {
        if (keyIsDown(CONTROL)) {
            zoomLevel = 20; 
        } else {
            zoomIn(); 
        }
    }

    if (key === '-' || key === '_') {
        if (keyIsDown(CONTROL)) {
            zoomLevel = maxZoomOutLevels[currentLevel]; 
        } else {
            zoomOut(); 
        }
    }
    if (key === '/' || key === '?') {
        showDebug = !showDebug;
        updateButtonState('debugToggle', showDebug);
        toggleOverlay();
    }
    if (key === 'Q' || key === 'q') {
        if (keyIsDown(CONTROL)) {
            if (!warpSpeed) {
                originalZoomLevel = zoomLevel;
                zoomLevel = 1;
                warpSpeed = true;  
            } else {
                zoomLevel = originalZoomLevel;
                warpSpeed = false;  
            }
        }
    } 
    if (key === 'T' || key === 't') {
        if (warpAllowed[currentLevel]) { 
            warpPlayer(); 
        } else {
            console.log('Warping not allowed until you land on a blue tile.');
        }
    }
    if (key === 'R' || key === 'r') {
        if (spawnAllowed[currentLevel]) { 
            addSpawnWithCooldown();
            updateButtonCooldown('spawn'); 
        } else {
            console.log('Spawning not allowed until you land on a green tile.');
        }
    }
    if (key === 'U' || key === 'u') createWormWithCooldown(); 
    if (key === 'Y' || key === 'y') saveCanvasImage();
    if (key === 'P' || key === 'p') saveCompositeImage(imgX, imgY);  
    if (key === 'G' || key === 'g') {
        gridToggle();
        updateButtonState('miniMap', showGrid);
    } 
    if (key === '[' || key === '{') previousLevel();
    if (key === ']' || key === '}') {
        if (magentaLanded[currentLevel]){
            nextLevel();
        } else if (keyIsDown(CONTROL)) {
            magentaLanded[currentLevel] = true;
            nextLevel();
        }
   }
}

function keyReleased() {
    keys[key.toLowerCase()] = false;
    if (keyCode === SHIFT) {
        shiftHeld = false;
        precisionModeActive = false; 
        updateButtonState('precisionMode', false);
    }
}

// MOUSE EVENTS
function mousePressed() {
    if (!buttonManager) return;

    buttonManager.handleMousePressed(mouseX, mouseY);
    
    if (buttonManager.buttons.some(button => button.isHovered(mouseX, mouseY))) {
        return false;
    }

    mouseHeld = true;

    const deltaX = mouseX - width / 2;
    const deltaY = mouseY - height / 2; 
    const angle = Math.atan2(deltaY, deltaX);

    mouseXDirection = Math.cos(angle);
    mouseYDirection = Math.sin(angle);
}

function mouseReleased() {
    mouseHeld = false;
}

function mouseMoved() {
    if (!buttonManager) return;

    hoveredIcon = null;

    buttonManager.handleMouseMoved(mouseX, mouseY);
}

function mouseOut() {
    hoveredIcon = null;
}

function isClickOnIcon(x, y) {
    let iconSize = 32;
    let padding = 10;

    return (x > padding && x < padding + iconSize) && 
           ((y > padding && y < padding + iconSize) || 
            (y > iconSize + 2 * padding && y < 2 * iconSize + 2 * padding));
}

function handleIconPress(x, y) {
    let iconSize = 32;
    let padding = 10;

    if (x > padding && x < padding + iconSize) {
        if (y > padding && y < padding + iconSize) {
            miniMapActive = !miniMapActive;
            gridToggle();
        } else if (y > iconSize + 2 * padding && y < 2 * iconSize + 2 * padding) {
            precisionModeActive = !precisionModeActive;
            togglePrecisionMode();
        }
    }
}

// TOUCH EVENTS 
function touchStarted() {
    if (!buttonManager) return;

    buttonManager.handleTouchStarted(touches[0].x, touches[0].y);
    
    if (buttonManager.buttons.some(button => button.isHovered(touches[0].x, touches[0].y))) {
        return false;
    }

    mouseHeld = true;

    const deltaX = touches[0].x - width / 2;
    const deltaY = touches[0].y - height / 2;
    const angle = Math.atan2(deltaY, deltaX);

    mouseXDirection = Math.cos(angle);
    mouseYDirection = Math.sin(angle);
}

function touchEnded() {
    mouseHeld = false;
}

/*
#################
UTILITY FUNCTIONS
#################
*/

// LEVEL MANAGEMENT
function setupLevel() {
    //console.log(`Setting up level: ${currentLevel}`);
    img = mapImages[currentLevel];
    
    if (!fogs[currentLevel]) {
        //console.log("Creating new fog layer for level", currentLevel);
        let newFog = createGraphics(img.width, img.height);
        newFog.pixelDensity(0.1);
        newFog.fill(120, 120, 120, 255);
        newFog.noStroke();
        newFog.rect(0, 0, newFog.width, newFog.height);
        fogs[currentLevel] = newFog;
    } else {
        //console.log("Using existing fog layer for level", currentLevel);
    }
    fog = fogs[currentLevel];

    if (!visitationGrids[currentLevel]) {
        //console.log("Creating new visitation grid for level", currentLevel);
        let newVisitationGrid = Array.from({ length: gridHeight }, () => Array(gridWidth).fill(false));
        visitationGrids[currentLevel] = newVisitationGrid;
    } else {
        //console.log("Using existing visitation grid for level", currentLevel);
    }
    visitGrid = visitationGrids[currentLevel];

    if (!playerPathLayers[currentLevel]) {
        //console.log("Creating new player path layer for level", currentLevel);
        let newPathLayer = createGraphics(img.width, img.height);
        newPathLayer.pixelDensity(1);
        newPathLayer.clear();
        playerPathLayers[currentLevel] = newPathLayer;
    } else {
        //console.log("Using existing player path layer for level", currentLevel);
    }
    playerPathLayer = playerPathLayers[currentLevel];

    if (!maxZoomOutLevels[currentLevel]) {
        maxZoomOutLevels[currentLevel] = 4; 
    }

    if (!warpButtonVisibles[currentLevel]) {
        warpButtonVisibles[currentLevel] = false; 
    }
    if (warpAllowed[currentLevel] === undefined) {
        warpAllowed[currentLevel] = false; 
    }

    if (!spawnAllowed[currentLevel]) {
        spawnAllowed[currentLevel] = false;
    }
    if (spawnAllowed[currentLevel]) {
        buttonManager.addButton(padding, 5 * padding + 4 * iconSize, iconSize, 'lime', 'red', 'spawn', addSpawnWithCooldown, null, spawnCooldownTime);
    } else {
        buttonManager.removeButton('spawn');
    }

    if (!warpAllowed[currentLevel]) {
        warpAllowed[currentLevel] = false;
        buttonManager.removeButton('warp');
    }

    zoomLevel = zoomLevels[currentLevel] !== undefined ? zoomLevels[currentLevel] : initialZoomLevel;

    if (!wormButtonVisibles[currentLevel]) {
        wormButtonVisibles[currentLevel] = false; // Initialize worm button visibility for each level
    }
    if (wormAllowed[currentLevel] === undefined) {
        wormAllowed[currentLevel] = false; // Initialize worm allowed for each level
    }

    if (wormButtonVisibles[currentLevel] || wormAllowed[currentLevel]) {
        buttonManager.addButton(padding, 7 * padding + 6 * iconSize, iconSize, 'yellow', 'yellow', 'createWorm', createWormWithCooldown);
    } else {
        buttonManager.removeButton('createWorm');
    }

    if (currentLevel > 0) {
        buttonManager.addButton(padding, 8 * padding + 7 * iconSize, iconSize, 'cyan', 'white', 'previousLevel', previousLevel);
    } else {
        buttonManager.removeButton('previousLevel');
    }

    if (currentLevel < highestLevelReached) {
        buttonManager.addButton(padding, 9 * padding + 8 * iconSize, iconSize, 'magenta', 'white', 'nextLevel', nextLevel);
    } else {
        buttonManager.removeButton('nextLevel');
    }

    if (magentaLanded[currentLevel] === undefined) {
        magentaLanded[currentLevel] = false;
    }
}




function nextLevel() {
    if (millis() - lastLevelChangeTime < levelChangeCooldown) return;
    if (currentLevel < mapImages.length - 1) {
        zoomLevels[currentLevel] = zoomLevel;
        worm = null;

        currentLevel++;
        img = mapImages[currentLevel];
        spawns = [];
        setupLevel(); 
        warpButtonVisible = false; 
        buttonManager.removeButton('warp');

        let validStart = false;
        while (!validStart) {
            let newImgX = int(random(max(0, imgX - 50), min(img.width, imgX + 50)));
            let newImgY = int(random(max(0, imgY - 50), min(img.height, imgY + 50)));
            let pixelColor = img.get(newImgX, newImgY);

            if (!(pixelColor[0] === 255 && pixelColor[1] === 0 && pixelColor[2] === 255) && 
                !(pixelColor[0] === 0 && pixelColor[1] === 255 && pixelColor[2] === 255) && 
                !(pixelColor[0] === 0 && pixelColor[1] === 0 && pixelColor[2] === 255)) {   
                imgX = newImgX;
                imgY = newImgY;
                validStart = true;
            }
        }

        if (currentLevel > highestLevelReached) {
            highestLevelReached = currentLevel;
        }

        lastLevelChangeTime = millis();

        if (wormButtonVisibles[currentLevel] || wormAllowed[currentLevel]) {
            buttonManager.addButton(padding, 7 * padding + 6 * iconSize, iconSize, 'yellow', 'yellow', 'createWorm', createWormWithCooldown);
        }

        if (warpAllowed[currentLevel]) {
            buttonManager.addButton(padding, 6 * padding + 5 * iconSize, iconSize, 'blue', 'white', 'warp', blueButtonAction, null, warpCooldownTime);
            warpButtonVisible = true;
        }
    }else{
        console.log(`BOTTOM FLOOR.`)
    }

    if (currentLevel > 0) {
        buttonManager.addButton(padding, 8 * padding + 7 * iconSize, iconSize, 'cyan', 'white', 'previousLevel', previousLevel);
    } else {
        buttonManager.removeButton('previousLevel');
    }

    if (currentLevel < highestLevelReached) {
        buttonManager.addButton(padding, 9 * padding + 8 * iconSize, iconSize, 'magenta', 'white', 'nextLevel', nextLevel);
    } else {
        buttonManager.removeButton('nextLevel');
    }

    zoomLevel = zoomLevels[currentLevel] !== undefined ? zoomLevels[currentLevel] : initialZoomLevel;
}


function previousLevel() {
    if (millis() - lastLevelChangeTime < levelChangeCooldown) return;
    if (currentLevel > 0) {
        zoomLevels[currentLevel] = zoomLevel;
        worm = null;

        currentLevel--;
        img = mapImages[currentLevel];
        spawns = [];
        setupLevel(); 
        warpButtonVisible = false; 
        buttonManager.removeButton('warp'); 

        let validStart = false;
        while (!validStart) {
            let newImgX = int(random(max(0, imgX - 50), min(img.width, imgX + 50)));
            let newImgY = int(random(max(0, imgY - 50), min(img.height, imgY + 50)));
            let pixelColor = img.get(newImgX, newImgY);

            if (!(pixelColor[0] === 255 && pixelColor[1] === 0 && pixelColor[2] === 255) && 
                !(pixelColor[0] === 0 && pixelColor[1] === 255 && pixelColor[2] === 255) && 
                !(pixelColor[0] === 0 && pixelColor[1] === 0 && pixelColor[2] === 255)) {   
                imgX = newImgX;
                imgY = newImgY;
                validStart = true;
            }
        }

        lastLevelChangeTime = millis(); 

      
        if (wormButtonVisibles[currentLevel] || wormAllowed[currentLevel]) {
            buttonManager.addButton(padding, 7 * padding + 6 * iconSize, iconSize, 'yellow', 'yellow', 'createWorm', createWormWithCooldown);
        }

        if (warpAllowed[currentLevel]) {
            buttonManager.addButton(padding, 6 * padding + 5 * iconSize, iconSize, 'blue', 'white', 'warp', blueButtonAction, null, warpCooldownTime);
            warpButtonVisible = true;
        }
    } else{
        console.log(`TOP FLOOR.`)
    }

    if (currentLevel > 0) {
        buttonManager.addButton(padding, 8 * padding + 7 * iconSize, iconSize, 'cyan', 'white', 'previousLevel', previousLevel);
    } else {
        buttonManager.removeButton('previousLevel');
    }

    if (currentLevel < highestLevelReached) {
        buttonManager.addButton(padding, 9 * padding + 8 * iconSize, iconSize, 'magenta', 'white', 'nextLevel');
    } else {
        buttonManager.removeButton('nextLevel');
    }

    zoomLevel = zoomLevels[currentLevel] !== undefined ? zoomLevels[currentLevel] : initialZoomLevel;
}


function extractMapName(filename) {
    let parts = filename.split('_');
    return parts[1];
}

function extractMapSeed(filename) {
    let parts = filename.split('_');
    let mapSeed = parts[8]; 
    return mapSeed; 
}

function extractMapInfo(filename,number){
    let parts = filename.split('_');
    return parts[number];
}

function updateMovement(pixelColor) {
    let currentMillis = millis();

    let colorString = `${pixelColor[0]},${pixelColor[1]},${pixelColor[2]}`;

    let gridX = Math.floor(imgX / gridSize);
    let gridY = Math.floor(imgY / gridSize);

    if (gridX >= 0 && gridX < gridWidth && gridY >= 0 && gridY < gridHeight) {
        if (!['magenta', 'cyan', 'red', 'white', 'blue', 'yellow'].includes(visitGrid[gridY][gridX])) {
            visitGrid[gridY][gridX] = true;
        }
    }

    let moveStep = normalSpeed;

    if (shiftHeld) {
        normalSpeed = 1;
        slowSpeed = 1;
        lastMoveTime = 0;
    } else if(warpSpeed) {
        normalSpeed = 15;
        slowSpeed = 15;
    } else {
        normalSpeed = 3;
        slowSpeed = 0.3;
    }

    switch (colorString) {
        // BLACK
        case "0,0,0":
            handleBlack(gridX, gridY, currentMillis);
            moveStep = slowSpeed;
            break;
        // MAGENTA
        case "255,0,255":
            handleMagenta(gridX, gridY, currentMillis);
            moveStep = slowSpeed / 2;
            break;
        // CYAN
        case "0,255,255":
            handleCyan(gridX, gridY, currentMillis);
            moveStep = slowSpeed / 2;
            break;
        // YELLOW
        case "255,255,0":
            handleYellow(gridX, gridY);
            break;
        // RED
        case "255,0,0":
            visitGrid[gridY][gridX] = 'red';
            if (maxZoomOutLevels[currentLevel] > 2) {
                maxZoomOutLevels[currentLevel] = 2; // Set max zoom out level to 2
            }
            break;
        // GREEN
        case "0,255,0":
            handleGreen(gridX, gridY);
            break;
        // BLUE
        case "0,0,255":
            handleBlue(currentMillis);
            moveStep = slowSpeed / 2;
            break;
        default:
            onBlack = false;
            onMagenta = false;
            onCyan = false;
            onBlue = false;
            break;
    }

    let moved = handleMovementInput(moveStep);

    if (moved) {
        updateGridForPosition(imgX, imgY);
        lastMoveTime = millis();
        autoExplore = false; 
    }


    handleAutoExplore(moveStep);
}

function handleBlack(gridX,gridY,currentMillis){
    onBlack = true;
}

function handleMagenta(gridX, gridY, currentMillis) {
    visitGrid[gridY][gridX] = 'magenta';
    if (!onMagenta) {
        onMagenta = true;
        magentaStartTime = currentMillis;
        magentaLanded[currentLevel] = true; 
        buttonManager.addButton(padding, 9 * padding + 8 * iconSize, iconSize, 'magenta', 'white', 'nextLevel', nextLevel);
    } else if (currentMillis - magentaStartTime > levelChangeCooldown) {
        nextLevel();
        lastLevelChangeTime = currentMillis;
        onMagenta = false;
    }
}

function handleCyan(gridX, gridY, currentMillis) {
    visitGrid[gridY][gridX] = 'cyan';
    if (!onCyan) {
        onCyan = true;
        cyanStartTime = currentMillis;
    } else if (currentMillis - cyanStartTime > levelChangeCooldown) {
        previousLevel();
        lastLevelChangeTime = currentMillis;
        onCyan = false;
    }
}

function handleGreen(gridX, gridY) {
    visitGrid[gridY][gridX] = 'white';
    if (!spawnAllowed[currentLevel]) {
        buttonManager.addButton(padding, 5 * padding + 4 * iconSize, iconSize, 'lime', 'red', 'spawn', addSpawnWithCooldown, null, spawnCooldownTime);
        spawnAllowed[currentLevel] = true; // Allow spawning on this level
    }
    if (millis() - lastSpawnTime > 250 && spawns.length < 50) {
        spawns.push(new Spawn(imgX, imgY));
        lastSpawnTime = millis();
    }
}

function handleBlue(currentMillis) {
    if (!onBlue) {
        onBlue = true;
        blueStartTime = millis();
        if (!warpAllowed[currentLevel]) {
            buttonManager.addButton(padding, 6 * padding + 5 * iconSize, iconSize, 'blue', 'blue', 'warp', blueButtonAction, null, warpCooldownTime);
            warpAllowed[currentLevel] = true; 
        }
    } else if (millis() - blueStartTime > 200) {
        if (currentMillis - lastWarpTime >= warpCooldownTime) {
            warpPlayer();
            lastWarpTime = currentMillis;
            blueStartTime = millis(); 
        }
    }
}

function handleYellow(gridX, gridY) {
    visitGrid[gridY][gridX] = 'yellow';
    moveStep = slowSpeed / 2;
    if (!wormButtonVisibles[currentLevel]) {
        buttonManager.addButton(padding, 7 * padding + 6 * iconSize, iconSize, 'yellow', 'yellow', 'createWorm', createWormWithCooldown);
        wormButtonVisibles[currentLevel] = true;
        wormAllowed[currentLevel] = true; 
    }
    if (previousZoomLevel === null) {
        previousZoomLevel = zoomLevel;
        zoomLevel = 1;
    }
    maxZoomOutLevels[currentLevel] = 1;
    createWormWithCooldown();
    zoomResetTime = millis() + 4000;
}

function handleMovementInput(moveStep) {
    let moved = false;
    if (keys['left'] || keys['a'] || keys['arrowleft']) { imgX -= moveStep; moved = true; }
    if (keys['right'] || keys['d'] || keys['arrowright']) { imgX += moveStep; moved = true; }
    if (keys['up'] || keys['w'] || keys['arrowup']) { imgY -= moveStep; moved = true; }
    if (keys['down'] || keys['s'] || keys['arrowdown']) { imgY += moveStep; moved = true; }
    if (mouseHeld) {
        imgX += moveStep * mouseXDirection;
        imgY += moveStep * mouseYDirection;
        moved = true;
    }
    return moved;
}

let previousGrid = { x: Math.floor(imgX / gridSize), y: Math.floor(imgY / gridSize) };

function handleAutoExplore(moveStep) {
    if (autoExplore) {
        let proposedX = imgX;
        let proposedY = imgY;
        let proposedGridX = Math.floor(imgX / gridSize);
        let proposedGridY = Math.floor(imgY / gridSize);

        if (directionChangeCooldown > 0) {
            directionChangeCooldown--;
        } else {
            autoExploreDirection = chooseDirectionBasedOnGrid(imgX, imgY);
            directionChangeCooldown = 30;
        }

        switch (autoExploreDirection) {
            case 0: proposedX += moveStep; break; // Right
            case 1: proposedY += moveStep; break; // Down
            case 2: proposedX -= moveStep; break; // Left
            case 3: proposedY -= moveStep; break; // Up
        }

        proposedGridX = Math.floor(proposedX / gridSize);
        proposedGridY = Math.floor(proposedY / gridSize);

        if (proposedGridX !== previousGrid.x || proposedGridY !== previousGrid.y) {
            //console.log(`AutoExplore heading to grid: (${proposedGridX}, ${proposedGridY})`);
        }

        if (proposedX >= 0 && proposedX <= img.width && proposedY >= 0 && proposedY <= img.height) {
            imgX = proposedX;
            imgY = proposedY;
            updateGridForPosition(imgX, imgY);

            let actualGridX = Math.floor(imgX / gridSize);
            let actualGridY = Math.floor(imgY / gridSize);

            if (actualGridX !== previousGrid.x || actualGridY !== previousGrid.y) {
                //console.log(`AutoExplore landed on grid: (${actualGridX}, ${actualGridY})`);
                previousGrid = { x: actualGridX, y: actualGridY };
            }
        }

        if (imgY >= img.height - gridSize) {
            autoExploreDirection = 3; // Up
        }

        if (imgX >= img.width - gridSize) {
            autoExploreDirection = 2; // Left
        }
    }
}


function drawOverlay() {
    if (!showOverlay) return;

    let margin = 40; 
    textSize(16);
    let lines = aboutText.trim().split('\n');
    let textWidthMax = 0;
    let textHeight = 0;

    for (let line of lines) {
        let lineWidth = textWidth(line);
        if (lineWidth > textWidthMax) {
            textWidthMax = lineWidth;
        }
        textHeight += textAscent() + textDescent();
    }

    let overlayWidth = textWidthMax + margin * 2;
    let overlayHeight = textHeight + margin * 2;
    let overlayX = (width - overlayWidth) / 2;
    let overlayY = (height - overlayHeight) / 2;

    fill(48, 48, 48, 240); 
    noStroke();
    rect(overlayX, overlayY, overlayWidth, overlayHeight);

    fill(255,255,255); 
    textAlign(CENTER, TOP);

    let textX = overlayX + overlayWidth / 2;
    let textY = overlayY + margin;

    for (let line of lines) {
        text(line, textX, textY);
        textY += textAscent() + textDescent();
    }
}


function toggleOverlay() {
    showOverlay = !showOverlay;
}

function checkBounds() {
    if (imgX < 0 || imgX > img.width || imgY < 0 || imgY > img.height) {
        if (isInBounds) {
            outOfBoundsStartTime = millis(); 
            isInBounds = false;
        } else if (millis() - outOfBoundsStartTime > 1000) { 
            teleportPlayerFromOutOfBounds();
        }
    } else {
            lastInBoundsPosition = { x: imgX, y: imgY }; 
    }
}

function teleportPlayerFromOutOfBounds() {
    if (lastInBoundsPosition.x <= 0) {
        imgX = 5;
    } else if (lastInBoundsPosition.x >= img.width) {
        imgX = img.width - 5;
    } else {
        imgX = int(constrain(lastInBoundsPosition.x, 0, img.width));
    }

    if (lastInBoundsPosition.y <= 0) {
        imgY = 5;
    } else if (lastInBoundsPosition.y >= img.height) {
        imgY = img.height - 5;
    } else {
        imgY = int(constrain(lastInBoundsPosition.y, 0, img.height));
    }

    console.log(`Player teleported to: ${imgX}, ${imgY}`);
    isInBounds = true;
}

function wasVisited(x, y) {
    let gridX = Math.floor(x / gridSize);
    let gridY = Math.floor(y / gridSize);

    if (gridX < 0 || gridX >= gridWidth || gridY < 0 || gridY >= gridHeight) {
        return false; 
    }
    
    return visitGrid[gridY][gridX];
}

function updateGridForPosition(x, y) {
    let gridX = Math.floor(x / gridSize);
    let gridY = Math.floor(y / gridSize);

    if (gridX >= 0 && gridX < gridWidth && gridY >= 0 && gridY < gridHeight) {
        let pixelColor = img.get(x, y);

        // Check for magenta (highest priority)
        if (pixelColor[0] === 255 && pixelColor[1] === 0 && pixelColor[2] === 255) {
            visitGrid[gridY][gridX] = 'magenta';
            //console.log(`Magenta cell entered at (${gridX}, ${gridY})`);
        }
        // Check for cyan
        else if (pixelColor[0] === 0 && pixelColor[1] === 255 && pixelColor[2] === 255) {
            visitGrid[gridY][gridX] = 'cyan';
            //console.log(`Cyan cell entered at (${gridX}, ${gridY})`);
        }
        // Check for yellow
        else if (pixelColor[0] === 255 && pixelColor[1] === 255 && pixelColor[2] === 0) {
            visitGrid[gridY][gridX] = 'yellow';
        }
        // Check for red
        else if (pixelColor[0] === 255 && pixelColor[1] === 0 && pixelColor[2] === 0) {
            visitGrid[gridY][gridX] = 'red';
            //console.log(`Red cell entered at (${gridX}, ${gridY})`);
        }
        // Check for green, mark it as white
        else if (pixelColor[0] === 0 && pixelColor[1] === 255 && pixelColor[2] === 0) {
            visitGrid[gridY][gridX] = 'white';
            //console.log(`White cell entered at (${gridX}, ${gridY})`);
        }
        // Check for blue (lowest priority among new colors)
        else if (pixelColor[0] === 0 && pixelColor[1] === 0 && pixelColor[2] === 255) {
            visitGrid[gridY][gridX] = 'blue';
            //console.log(`Blue cell entered at (${gridX}, ${gridY})`);
        }
        // Default to visited
        else if (visitGrid[gridY][gridX] !== 'blue' && visitGrid[gridY][gridX] !== 'magenta' &&
                 visitGrid[gridY][gridX] !== 'cyan' && visitGrid[gridY][gridX] !== 'red' && 
                 visitGrid[gridY][gridX] !== 'white' && visitGrid[gridY][gridX] !== 'yellow') {
            visitGrid[gridY][gridX] = true;
        }
        lastUpdatedCell = { x: gridX, y: gridY }; // Update last updated cell
    }
}

function chooseDirectionBasedOnGrid(x, y) {
    let directions = [
        { dir: 0, x: x + gridSize, y: y }, // Right
        { dir: 1, x: x, y: y + gridSize }, // Down
        { dir: 2, x: x - gridSize, y: y }, // Left
        { dir: 3, x: x, y: y - gridSize }  // Up
    ];

    let validDirections = directions.filter(d => 
        d.x >= 0 && d.x <= img.width && d.y >= 0 && d.y <= img.height && !wasVisited(d.x, d.y)
    );

    if (validDirections.length > 0) {
        return validDirections[Math.floor(Math.random() * validDirections.length)].dir;
    }

    return Math.floor(Math.random() * 4);
}

function calculateExplorationPercentage() {
    let totalCells = gridWidth * gridHeight;
    let visitedCells = 0;
    for (let i = 0; i < gridHeight; i++) {
        for (let j = 0; j < gridWidth; j++) {
            if (visitGrid[i][j] === true || typeof visitGrid[i][j] === 'string') { 
                visitedCells++;
            }
        }
    }
    return (visitedCells / totalCells) * 100;
}

function drawExplorationGrid(drawSurface = window, startX = width - 266, startY = height - 266, scale = 1) {
    const gridDisplaySize = 256 * scale;
    const cellSize = gridDisplaySize / gridWidth;
    let exploredPercentage = calculateExplorationPercentage(); 

    drawSurface.textSize(16 * scale);
    drawSurface.fill(255);
    drawSurface.noStroke();
    drawSurface.textAlign(LEFT, TOP);
    drawSurface.text(`MAP: ${mapNames[currentLevel]}`, startX, startY - 60 * scale);
    drawSurface.text(`SEED: ${mapSeeds[currentLevel]}`, startX, startY - 40 * scale);
    drawSurface.text(`[${currentLevel + 1}:${mapImages.length}] [${round(imgX)},${round(imgY)}] [${zoomLevel}x] [${exploredPercentage.toFixed(2)}%]`, startX, startY - 20 * scale);

    for (let i = 0; i < gridHeight; i++) {
        for (let j = 0; j < gridWidth; j++) {
            let colorValue = getColorForCell(visitGrid[i][j], i, j);
            drawSurface.fill(colorValue);
            drawSurface.noStroke();
            const x = startX + j * cellSize;
            const y = startY + i * cellSize;
            drawSurface.rect(x, y, cellSize, cellSize);
        }
    }
}

function getColorForCell(cell, gridY, gridX) {
    if (cell === 'magenta') {
        return color(255, 0, 255); // Magenta for magenta cells
    } else if (cell === 'cyan') {
        return color(0, 255, 255); // Cyan for cyan cells
    } else if (cell === 'red') {
        return color(255, 0, 0); // Red for red cells
    } else if (cell === 'white') {
        return color(255, 255, 255); // White for white cells
    } else if (cell === 'blue') {
        return color(0, 0, 255); // Blue for blue cells
    } else if (cell === 'yellow') {
        return color(255, 255, 0); // Yellow for yellow cells
    } else if (lastUpdatedCell.x === gridX && lastUpdatedCell.y === gridY) {
        return color(255, 0, 255); // Magenta for the most recently updated cell
    } else {
        return cell ? color(0, 255, 0) : color(0, 0, 0); // Green if visited, black if not
    }
}



let fpsAverage = 0;
let frameCount = 0;
let totalFPS = 0;
let averagingFrames = 60; 

function displayDebug(pixelColor) {
    let currentSpeed = (pixelColor[0] === 0 && pixelColor[1] === 0 && pixelColor[2] === 0) ? slowSpeed : normalSpeed;
    if (pixelColor[0] === 0 && pixelColor[1] === 0 && pixelColor[2] === 255) {
        currentSpeed = slowSpeed; 
    }

    totalFPS += frameRate();
    frameCount++;

    if (frameCount >= averagingFrames) {
        fpsAverage = totalFPS / frameCount;
        totalFPS = 0;  // Reset totalFPS
        frameCount = 0;  // Reset frame count
    }
    
    let elapsedTime = millis();
    let hours = Math.floor(elapsedTime / 3600000); 
    let minutes = Math.floor((elapsedTime % 3600000) / 60000); 
    let seconds = Math.floor((elapsedTime % 60000) / 1000); 

    let formattedTime = nf(hours, 2) + ":" + nf(minutes, 2) + ":" + nf(seconds, 2);

    fill(120, 120, 120, 0); 
    noStroke();
    rectMode(CORNER);
    rect(width - 200, 0, 200, 200, 20); 

    textSize(16);
    fill(255);
    textAlign(RIGHT, TOP);

    text(`FPS: ${Math.round(fpsAverage)}`, width - 10, 10);
    text(`ZOOM: x${zoomLevel}`, width - 10, 30);
    text(`COLOR: [${pixelColor}]`, width - 10, 50);
    text(`XY: (${Math.round(imgX)}, ${Math.round(imgY)})`, width - 10, 70);
    text(`SPEED: ${currentSpeed}`, width - 10, 90);
    text(`SPAWNS: ${spawns.length}`, width - 10, 110); 
    if (performance && performance.memory) {
        let usedHeap = performance.memory.usedJSHeapSize / 1048576; 
        let totalHeap = performance.memory.totalJSHeapSize / 1048576;
        text(`MEMORY USED: ${usedHeap.toFixed(2)} MB`, width - 10, 130);
        text(`TOTAL HEAP: ${totalHeap.toFixed(2)} MB`, width - 10, 150);
    }
    text(`TIME RUNNING: ${formattedTime}`, width - 10, 170);
}

// FOG MANAGEMENT
function clearFog(pixelColor) {
    let clearSize = 100;

    // RED
    if (pixelColor[0] === 255 && pixelColor[1] === 0 && pixelColor[2] === 0) {
        clearSize *= 3; // Triple the clear size if the pixel is red

        if (previousZoomLevel === null) {
            previousZoomLevel = zoomLevel;
            zoomLevel = 2;
        }
        zoomResetTime = millis() + 4000; // Continuously reset the zoom reset time to 5 seconds in the future
    }

    let centerX = Math.floor(imgX - clearSize / 2);
    let centerY = Math.floor(imgY - clearSize / 2);

    fog.erase();
        fog.noStroke();
        rectMode(CENTER);
        fog.rect(centerX, centerY, clearSize, clearSize);
    fog.noErase();
}

/*
#################
BUTTON MANAGEMENT
#################
*/

class Button {
    constructor(x, y, size, color, activeColor, label, action, pattern = null, cooldown = 0) {
        this.x = x;
        this.y = y;
        this.size = size;
        this.color = color;
        this.activeColor = activeColor;
        this.label = label;
        this.action = action;
        this.active = false;
        this.pattern = pattern; 
        this.highlight = false; 
        this.cooldown = cooldown; 
        this.lastActionTime = 0; 
    }

    draw() {
        rectMode(CORNER);
        fill(this.active ? this.activeColor : this.color);
        rect(this.x, this.y, this.size, this.size);
        
        if ((this.label === 'createWorm' && worm && worm.alive) || this.isOnCooldown()) {
            this.drawPattern(patternArraySlash); // Apply pattern if the worm is alive
        } else if (this.pattern && this.active) {
            this.drawPattern(this.pattern);
        } else if (this.active) {
            fill(this.color);
            let smallSize = this.size / 2;
            rect(this.x + (this.size - smallSize) / 2, this.y + (this.size - smallSize) / 2, smallSize, smallSize);
        }

        if (this.highlight && (this.label === 'spawn' || this.label === 'warp' || this.label === 'createWorm')) {
            fill('red');
            let smallSize = this.size / 2;
            rect(this.x + (this.size - smallSize) / 2, this.y + (this.size - smallSize) / 2, smallSize, smallSize);
        }
        if (this.highlight && (this.label === 'zoomIn' || this.label === 'zoomOut' || this.label === 'saveComposite')) {
            fill('magenta');
            let smallSize = this.size / 2;
            rect(this.x + (this.size - smallSize) / 2, this.y + (this.size - smallSize) / 2, smallSize, smallSize);
        }
    }

    drawPattern(pattern) {
        let pixelSize = this.size / pattern.length;
        for (let y = 0; y < pattern.length; y++) {
            for (let x = 0; x < pattern[y].length; x++) {
                fill(pattern[y][x] === 1 ? 'black' : this.color);
                rect(this.x + x * pixelSize, this.y + y * pixelSize, pixelSize, pixelSize);
            }
        }
    }

    isHovered(mouseX, mouseY) {
        return mouseX > this.x && mouseX < this.x + this.size &&
               mouseY > this.y && mouseY < this.y + this.size;
    }

    toggle() {
        let currentMillis = millis();
        if (!this.isOnCooldown()) {
            this.action();
            this.lastActionTime = currentMillis;
        }
        if (this.label === 'spawn' || this.label === 'warp' || this.label === 'createWorm' || this.label === 'zoomIn' || this.label === 'zoomOut' || this.label === 'saveComposite') {
            this.highlight = true;
            setTimeout(() => {
                this.highlight = false;
            }, 100);
        }  else {
            this.active = !this.active;
        }
    }

    isOnCooldown() {
        let currentMillis = millis();
        return (currentMillis - this.lastActionTime) < this.cooldown;
    }
}

class ButtonManager {
    constructor() {
        this.buttons = [];
    }

    addButton(x, y, size, color, activeColor, label, action, pattern = null, cooldown = 0) {
        let button = new Button(x, y, size, color, activeColor, label, action, pattern, cooldown);
        this.buttons.push(button);
    }

    removeButton(label) {
        this.buttons = this.buttons.filter(button => button.label !== label);
    }

    drawButtons() {
        this.buttons.forEach(button => button.draw());
    }

    handleMousePressed(mouseX, mouseY) {
        this.buttons.forEach(button => {
            if (button.isHovered(mouseX, mouseY)) {
                button.toggle();
            }
        });
    }

    handleMouseMoved(mouseX, mouseY) {
        this.buttons.forEach(button => {
            if (button.isHovered(mouseX, mouseY)) {
                hoveredIcon = button.label;
            }
        });
    }

    handleTouchStarted(touchX, touchY) {
        this.buttons.forEach(button => {
            if (button.isHovered(touchX, touchY)) {
                button.toggle();
            }
        });
    }
}

function updateButtonState(label, state) {
    buttonManager.buttons.forEach(button => {
        if (button.label === label) {
            button.active = state;
        }
    });
}

function updateButtonCooldown(label) {
    buttonManager.buttons.forEach(button => {
        if (button.label === label) {
            button.lastActionTime = millis();
        }
    });
}

// BUTTON ACTIONS
function togglePrecisionMode() {
    shiftHeld = !shiftHeld;
    precisionModeActive = shiftHeld; 
    if (shiftHeld) {
        autoExplore = false;
        console.log('Precision mode enabled');
    } else {
        lastMoveTime = millis(); 
        console.log('Precision mode disabled');
    }
}

function warpPlayer() {
    if (!warpAllowed[currentLevel]) {
        console.log('Warping not allowed until you land on a blue tile.');
        return;
    }

    let currentMillis = millis();
    if (currentMillis - lastWarpTime >= warpCooldownTime) {
        let validWarp = false;
        while (!validWarp) {
            let newImgX = int(random(0, img.width));
            let newImgY = int(random(0, img.height));
            let pixelColor = img.get(newImgX, newImgY);

            // Ensure the pixel is not black, magenta, cyan, or blue
            if (!(pixelColor[0] === 0 && pixelColor[1] === 0 && pixelColor[2] === 0) &&   // Black
                !(pixelColor[0] === 255 && pixelColor[1] === 0 && pixelColor[2] === 255) && // Magenta
                !(pixelColor[0] === 0 && pixelColor[1] === 255 && pixelColor[2] === 255) && // Cyan
                !(pixelColor[0] === 0 && pixelColor[1] === 0 && pixelColor[2] === 255)) {   // Blue
                imgX = newImgX;
                imgY = newImgY;
                validWarp = true;
            }
        }
        lastWarpTime = currentMillis;
        updateButtonCooldown('warp'); 
        console.log('Player warped to:', imgX, imgY);
    } else {
        console.log(`Warp on cooldown. Please wait ${(warpCooldownTime - (currentMillis - lastWarpTime)) / 1000} seconds.`);
    }
}


function addSpawn() {
    spawns.push(new Spawn(imgX, imgY));
    console.log('New spawn added at:', imgX, imgY);
}

function addSpawnWithCooldown() {
    let currentMillis = millis();
    if (currentMillis - lastSpawnTime >= spawnCooldownTime) {
        addSpawn(); // Add a new spawn
        lastSpawnTime = currentMillis;
    } else {
        console.log(`Spawn on cooldown. Please wait ${(spawnCooldownTime - (currentMillis - lastSpawnTime)) / 1000} seconds.`);
    }
}

function addSpawnWithHighlight() {
    addSpawn();
    buttonManager.buttons.forEach(button => {
        if (button.label === 'spawn') {
            button.highlight = true;
            setTimeout(() => {
                button.highlight = false;
            }, 100); 
        }
    });
}

function blueButtonAction() {
    let currentMillis = millis();
    if (currentMillis - lastWarpTime >= warpCooldownTime) {
        warpPlayer();
        lastWarpTime = currentMillis;
    } else {
        console.log(`Warp on cooldown. Please wait ${(warpCooldownTime - (currentMillis - lastWarpTime)) / 1000} seconds.`);
    }
}

function createWorm(x, y) {
    if (!worm || !worm.alive) {
        worm = new Worm(x, y);
        console.log('New worm created at:', x, y);
    } else {
        console.log('Previous worm is still alive.');
    }
}

function createWormWithCooldown() {
    let currentMillis = millis();

    if (wormAllowed[currentLevel]) {
        if (!worm || !worm.alive || (worm && currentMillis - worm.birthTime >= worm.cooldownTime)) {
            createWorm(imgX, imgY);
            lastWormTime = currentMillis;
        } else {
            let remainingCooldown = worm.cooldownTime - (currentMillis - worm.birthTime);
            console.log(`Worm creation on cooldown. Please wait ${remainingCooldown / 1000} seconds.`);
        }
    } else {
        console.log('Worm creation not allowed on this floor. Unlock by landing on a yellow tile.');
    }
}

function debugToggle() {
    showDebug = !showDebug;
    toggleOverlay(); 
}

function gridToggle() {
    showGrid = !showGrid;
}

function zoomIn() {
    zoomLevel = min(zoomLevel + 1, 20); 
    console.log('Zoomed In: Level ' + zoomLevel);
}

function zoomOut() {
    zoomLevel = max(zoomLevel - 1, maxZoomOutLevels[currentLevel]); 
    console.log('Zoomed Out: Level ' + zoomLevel);
}

function saveCompositeImage(playerX = imgX, playerY = imgY) {
    let compositeImage = createGraphics(5120, 5120);
    compositeImage.pixelDensity(pixelDensity()); 
    compositeImage.noSmooth(); // 
    compositeImage.background(0);
    compositeImage.textFont(unifont);

    let screenshotSize = 256;
    let screenshot = createGraphics(screenshotSize, screenshotSize);
    screenshot.noSmooth();
    
    let centerX = max(0, min(playerX - screenshotSize / 2, img.width - screenshotSize));
    let centerY = max(0, min(playerY - screenshotSize / 2, img.height - screenshotSize));

    centerX = constrain(centerX, 0, img.width - screenshotSize);
    centerY = constrain(centerY, 0, img.height - screenshotSize);

    screenshot.image(img, 0, 0, screenshotSize, screenshotSize, centerX, centerY, screenshotSize, screenshotSize);

    screenshot.image(fog, 0, 0, screenshotSize, screenshotSize, centerX, centerY, screenshotSize, screenshotSize);

    screenshot.image(playerPathLayer, 0, 0, screenshotSize, screenshotSize, centerX, centerY, screenshotSize, screenshotSize);

    spawns.forEach(spawn => {
        let spawnX = (spawn.x - centerX);
        let spawnY = (spawn.y - centerY);
        screenshot.fill(255, 0, 0); 
        screenshot.noStroke();
        screenshot.rectMode(CENTER);
        screenshot.rect(spawnX, spawnY, spawn.size, spawn.size);
    });

    if (worm) {
        worm.segments.forEach(segment => {
            let segmentX = (segment.x - centerX);
            let segmentY = (segment.y - centerY);
            screenshot.fill(255, 255, 0); 
            screenshot.noStroke();
            screenshot.rectMode(CENTER);
            screenshot.rect(segmentX, segmentY, 10, 10);
        });
    }

    let playerColor;
    if (onMagenta || onCyan || onBlue) {
        playerColor = color(255, 255, 0); 
    } else if (onBlack) {
        playerColor = color(255, 255, 255); 
    } else {
        playerColor = color(255, 0, 255);
    }

    let playerDrawX = screenshotSize / 2 + (playerX - centerX - screenshotSize / 2);
    let playerDrawY = screenshotSize / 2 + (playerY - centerY - screenshotSize / 2);

    screenshot.fill(playerColor);
    screenshot.rectMode(CENTER);
    screenshot.noStroke();
    let playerSize = 5; 
    screenshot.rect(playerDrawX, playerDrawY, playerSize, playerSize);

    if (precisionModeActive) {
        screenshot.fill(0, 255, 0); 
        let greenRectSize = playerSize / 2;
        screenshot.rect(playerDrawX, playerDrawY, greenRectSize, greenRectSize);
    }

    let scaleFactor = 2560 / screenshotSize;
    let scaledScreenshot = createGraphics(screenshotSize * scaleFactor, screenshotSize * scaleFactor);
    scaledScreenshot.noSmooth(); 
    scaledScreenshot.image(screenshot, 0, 0, screenshotSize * scaleFactor, screenshotSize * scaleFactor);

    compositeImage.image(scaledScreenshot, 0, 0);
    compositeImage.rectMode(CORNER);
    compositeImage.noStroke();
    compositeImage.fill(0, 0, 0, 0)

    let gridScale = 7.5;
    let gridTopLeftX = compositeImage.width - 266*gridScale; 
    let gridTopLeftY = compositeImage.height - 266*gridScale; 

    drawExplorationGrid(compositeImage, gridTopLeftX, gridTopLeftY,gridScale)
    let exploredPercentage = calculateExplorationPercentage();


    let miniMapSize = img.width; 
    let miniMap = createGraphics(miniMapSize, miniMapSize);
    miniMap.noSmooth();
    miniMap.image(img, 0, 0, miniMapSize, miniMapSize, 0, 0, img.width, img.height);

    let mapX = compositeImage.width - miniMapSize;  
    let mapY = 0;  

    compositeImage.image(miniMap, mapX, mapY);

    let miniMapFogSize = fog.width; 
    let miniMapFog = createGraphics(miniMapFogSize, miniMapFogSize);
    miniMapFog.noSmooth();
    miniMapFog.image(fog, 0, 0, miniMapSize, miniMapSize, 0, 0, img.width, img.height);

    let mapXFog = compositeImage.width - miniMapFogSize ; 
    let mapYFog = 0; 

    compositeImage.image(miniMapFog, mapXFog, mapYFog);

    let visitedGrid = saveTextBasedVisitedGrid_02(); 
    compositeImage.image(visitedGrid, 0, 2560);

    let timestamp = getFormattedDateTime();
    saveCanvas(compositeImage, `${timestamp}_${mapNames[currentLevel]}_[${currentLevel + 1}OF${mapImages.length}]_[${round(imgX)},${round(imgY)}]_[${int(exploredPercentage)}%]`,`png`);
    console.log('Composite image saved');
}

function saveTextBasedVisitedGrid_02() {
    let canvasSize = 2560; 
    let cellSize = canvasSize / gridWidth; 

    let gridCanvas = createGraphics(canvasSize, canvasSize);
    gridCanvas.background(255); 
    gridCanvas.noStroke();
    gridCanvas.noSmooth();

    gridCanvas.textFont(unifont);
    gridCanvas.textSize(cellSize / 1.2);
    gridCanvas.textAlign(CENTER, CENTER);

    for (let y = 0; y < gridHeight; y++) {
        for (let x = 0; x < gridWidth; x++) {
            gridCanvas.fill(getColorForCell(visitGrid[y][x], y, x));

            gridCanvas.rect(x * cellSize, y * cellSize, cellSize, cellSize);

            let textToShow = visitGrid[y][x] ? '1' : '0'; 
            gridCanvas.fill(255); 
            let textNudge = 3;
            gridCanvas.text(textToShow, x * cellSize + cellSize / 2, y * cellSize + cellSize / 2 - textNudge);
        }
    }

    return gridCanvas;
}


function printScreen() {
    let timestamp = getFormattedDateTime();
    window.save(`Screenshot of CAVE ${currentLevel + 1} of ${mapImages.length} ${timestamp}`);
}


/*
#################
SPAWN MANAGEMENT
#################
*/
class Spawn {
    constructor(x, y) {
        this.x = x;
        this.y = y;
        this.angle = random(TWO_PI); 
        this.radius = 1; 
        this.waveAmplitude = random(5,30); 
        this.waveFrequency = random(0.01,0.5);
        this.lifeTime = random(2000,20000); 
        this.spawnTime = millis();
        this.size = random(1,5);
        this.movementType = floor(random(6));
    }

    update() {
        switch (this.movementType) {
            case 0:
                this.randomWalk();
                break;
            case 1:
                this.straightLine();
                break;
            case 2:
                this.tightSpiral();
                break;
            case 3:
                this.expandingSpiral();
                break;
            case 4:
                this.tightWave();
                break;
            case 5:
                this.wideWave();
                break;
        }

        this.clearFog();
        this.updateVisitedGrid();

        if (millis() - this.spawnTime > this.lifeTime) {
            spawns.splice(spawns.indexOf(this), 1);
        }
    }

    randomWalk() {
        let moveStep = 3;
        let direction = floor(random(4));
        switch (direction) {
            case 0: this.x += moveStep; break;
            case 1: this.x -= moveStep; break;
            case 2: this.y += moveStep; break;
            case 3: this.y -= moveStep; break;
        }
    }

    straightLine() {
        let moveStep = 1;
        this.x += moveStep * cos(this.angle);
        this.y += moveStep * sin(this.angle);
    }

    tightSpiral() {
        let moveStep = 1;
        this.x += (this.radius * cos(this.angle));
        this.y += (this.radius * sin(this.angle));
        this.angle += radians(random(8,12)); 
        this.radius += random(0.01,0.20);
    }

    expandingSpiral() {
        let moveStep = 1;
        this.x += (this.radius * cos(this.angle));
        this.y += (this.radius * sin(this.angle));
        this.angle += radians(random(3,7)); 
        this.radius += random(0.01,0.20); 
    }

    tightWave() {
        let moveStep = slowSpeed;
        this.x += moveStep;
        this.y += this.waveAmplitude * sin(this.angle); 
        this.angle += this.waveFrequency; 
    }

    wideWave() {
        let moveStep = slowSpeed;
        this.y += moveStep; 
        this.x += this.waveAmplitude * cos(this.angle); 
        this.angle += this.waveFrequency; 
    }

    warp() {
        this.x = random(0, img.width);
        this.y = random(0, img.height);
    }

    clearFog() {
        let clearSize = 10; 
        fog.erase();
        fog.rect(this.x - clearSize / 2, this.y - clearSize / 2, clearSize, clearSize);
        fog.noErase();

    }

    updateVisitedGrid() {
        let gridX = Math.floor(this.x / gridSize);
        let gridY = Math.floor(this.y / gridSize);

        if (gridX >= 0 && gridX < gridWidth && gridY >= 0 && gridY < gridHeight) {
            let pixelColor = img.get(this.x, this.y);

            // Check for magenta (highest priority)
            if (pixelColor[0] === 255 && pixelColor[1] === 0 && pixelColor[2] === 255) {
                visitGrid[gridY][gridX] = 'magenta';
                //console.log(`Magenta cell entered at (${gridX}, ${gridY})`);
            }
            // Check for cyan
            else if (pixelColor[0] === 0 && pixelColor[1] === 255 && pixelColor[2] === 255) {
                visitGrid[gridY][gridX] = 'cyan';
                //console.log(`Cyan cell entered at (${gridX}, ${gridY})`);
            }
            // Check for yellow
            else if (pixelColor[0] === 255 && pixelColor[1] === 255 && pixelColor[2] === 0) {
                visitGrid[gridY][gridX] = 'yellow';
            }
            // Check for red
            else if (pixelColor[0] === 255 && pixelColor[1] === 0 && pixelColor[2] === 0) {
                visitGrid[gridY][gridX] = 'red';
                //console.log(`Red cell entered at (${gridX}, ${gridY})`);
            }
            // Check for green, mark it as white
            else if (pixelColor[0] === 0 && pixelColor[1] === 255 && pixelColor[2] === 0) {
                visitGrid[gridY][gridX] = 'white';
                //console.log(`White cell entered at (${gridX}, ${gridY})`);
            }
            // Check for blue (lowest priority among new colors)
            else if (pixelColor[0] === 0 && pixelColor[1] === 0 && pixelColor[2] === 255) {
                visitGrid[gridY][gridX] = 'blue';
                //console.log(`Blue cell entered at (${gridX}, ${gridY})`);
            }
            // Default to visited
            else if (visitGrid[gridY][gridX] !== 'blue' && visitGrid[gridY][gridX] !== 'magenta' &&
                     visitGrid[gridY][gridX] !== 'cyan' && visitGrid[gridY][gridX] !== 'red' && 
                     visitGrid[gridY][gridX] !== 'white' && visitGrid[gridY][gridX] !== 'yellow') {
                visitGrid[gridY][gridX] = true;
            }
            lastUpdatedCell = { x: gridX, y: gridY };
        }
    }

    draw() {
        let viewWidth = width / zoomLevel;
        let viewHeight = height / zoomLevel;
        let viewX = imgX - viewWidth / 2;
        let viewY = imgY - viewHeight / 2;

        let drawX = (this.x - viewX) * zoomLevel; 
        let drawY = (this.y - viewY) * zoomLevel; 

        fill(255, 0, 0); // Red color for spawns
        noStroke();
        rectMode(CENTER);
        rect(drawX, drawY, this.size * zoomLevel, this.size * zoomLevel);
    }
}

class Worm {
    constructor(x, y) {
        this.x = x;
        this.y = y;
        this.segments = [];
        for (let i = 0; i < 35; i++) {
            this.segments.push({ x: x, y: y });
        }
        this.speed = 4; 
        this.directions = [createVector(1, 0), createVector(-1, 0), createVector(0, 1), createVector(0, -1)]; 
        this.currentDirection = random(this.directions);
        this.lastDirectionChange = millis();
        this.lifespan = random(2000, 10000); 
        this.birthTime = millis(); 
        this.alive = true; 
        this.cooldownTime = this.lifespan;
    }

    update() {
        if (!this.alive) return;

        if (millis() - this.lastDirectionChange > random(250, 2500)) {
            let possibleDirections = this.directions.filter(dir => 
                (dir.x !== -this.currentDirection.x || dir.y !== -this.currentDirection.y)
            );
            this.currentDirection = random(possibleDirections);
            this.lastDirectionChange = millis();
        }

        let newHeadX = this.segments[0].x + this.currentDirection.x * this.speed;
        let newHeadY = this.segments[0].y + this.currentDirection.y * this.speed;

        for (let i = this.segments.length - 1; i > 0; i--) {
            this.segments[i] = { ...this.segments[i - 1] };
        }

        this.segments[0] = { x: newHeadX, y: newHeadY };

        this.clearFog(newHeadX,newHeadY);

        this.updateVisitedGrid(this.segments[0].x, this.segments[0].y);

        if (millis() - this.birthTime > this.lifespan) {
            this.alive = false;
        }
    }

    updateVisitedGrid(x,y) {
        let gridX = Math.floor(x / gridSize);
        let gridY = Math.floor(y / gridSize);

        if (gridX >= 0 && gridX < gridWidth && gridY >= 0 && gridY < gridHeight) {
            let pixelColor = img.get(x, y);

            // Check for magenta (highest priority)
            if (pixelColor[0] === 255 && pixelColor[1] === 0 && pixelColor[2] === 255) {
                visitGrid[gridY][gridX] = 'magenta';
                //console.log(`Magenta cell entered at (${gridX}, ${gridY})`);
            }
            // Check for cyan
            else if (pixelColor[0] === 0 && pixelColor[1] === 255 && pixelColor[2] === 255) {
                visitGrid[gridY][gridX] = 'cyan';
                //console.log(`Cyan cell entered at (${gridX}, ${gridY})`);
            }
            // Check for yellow
            else if (pixelColor[0] === 255 && pixelColor[1] === 255 && pixelColor[2] === 0) {
                visitGrid[gridY][gridX] = 'yellow';
            }
            // Check for red
            else if (pixelColor[0] === 255 && pixelColor[1] === 0 && pixelColor[2] === 0) {
                visitGrid[gridY][gridX] = 'red';
                //console.log(`Red cell entered at (${gridX}, ${gridY})`);
            }
            // Check for green, mark it as white
            else if (pixelColor[0] === 0 && pixelColor[1] === 255 && pixelColor[2] === 0) {
                visitGrid[gridY][gridX] = 'white';
                //console.log(`White cell entered at (${gridX}, ${gridY})`);
            }
            // Check for blue (lowest priority among new colors)
            else if (pixelColor[0] === 0 && pixelColor[1] === 0 && pixelColor[2] === 255) {
                visitGrid[gridY][gridX] = 'blue';
                //console.log(`Blue cell entered at (${gridX}, ${gridY})`);
            }
            // Default to visited
            else if (visitGrid[gridY][gridX] !== 'blue' && visitGrid[gridY][gridX] !== 'magenta' &&
                     visitGrid[gridY][gridX] !== 'cyan' && visitGrid[gridY][gridX] !== 'red' && 
                     visitGrid[gridY][gridX] !== 'white' && visitGrid[gridY][gridX] !== 'yellow') {
                visitGrid[gridY][gridX] = true;
            }
            lastUpdatedCell = { x: gridX, y: gridY };
        }
    }

    draw(viewX, viewY, zoomLevel) {
        if (!this.alive) return;

        fill(255, 255, 0);
        noStroke();
        for (let segment of this.segments) {
            let drawX = (segment.x - viewX) * zoomLevel;
            let drawY = (segment.y - viewY) * zoomLevel;
            rect(drawX, drawY, 10 * zoomLevel, 10 * zoomLevel);
        }
        this.checkAndChangePixels(this.segments[0].x, this.segments[0].y);
    }

    clearFog(x,y) {
        let clearSize = 5; 
        fog.erase();
        fog.rect(x - clearSize / 2, y - clearSize / 2, clearSize, clearSize);
        fog.noErase();

    }

    checkAndChangePixels(x, y) {
        const pathRadius = 10;

        for (let i = -pathRadius; i <= pathRadius; i++) {
            for (let j = -pathRadius; j <= pathRadius; j++) {
                const distance = dist(0, 0, i, j);
                if (distance <= pathRadius) {
                    const sampleX = Math.round(x + i);
                    const sampleY = Math.round(y + j);

                    if (sampleX >= 0 && sampleX < img.width && sampleY >= 0 && sampleY < img.height) {
                        const pixelColor = img.get(sampleX, sampleY);
                        // Check if the pixel is black
                        if (pixelColor[0] === 0 && pixelColor[1] === 0 && pixelColor[2] === 0) {
                            playerPathLayer.noStroke();
                            playerPathLayer.fill(255, 255, 255); // Change to white
                            playerPathLayer.rect(sampleX, sampleY, 1, 1);
                        }
                    }
                }
            }
        }
    }
}


/*
#######################
MISCELLANEOUS FUNCTIONS
#######################
*/
function getFormattedDateTime() {
    const now = new Date();
    const year = now.getFullYear();
    const month = now.getMonth() + 1; // getMonth() is zero-indexed
    const day = now.getDate();
    const hours = now.getHours();
    const minutes = now.getMinutes();
    const seconds = now.getSeconds();
    return `${year}-${month}-${day}_${hours}-${minutes}-${seconds}`;
}

function isMobile() {
    return /Mobi|Android|iPhone|iPad|iPod|Opera Mini|IEMobile|WPDesktop|Windows Phone|webOS|BlackBerry|BB10|PlayBook|Silk|Kindle|Mobile|Tablet/i.test(navigator.userAgent);
}
